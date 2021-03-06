package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"go/format"
	"io/ioutil"
	"sort"
	"strings"
	"text/template"

	"github.com/BurntSushi/toml"
	"github.com/achiku/varfmt"
	"github.com/lib/pq" // postgres
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// Queryer database/sql compatible query interface
type Queryer interface {
	Exec(string, ...interface{}) (sql.Result, error)
	Query(string, ...interface{}) (*sql.Rows, error)
	QueryRow(string, ...interface{}) *sql.Row
}

// OpenDB opens database connection
func OpenDB(connStr string) (*sql.DB, error) {
	conn, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, errors.Wrap(err, "failed to connect to database")
	}
	return conn, nil
}

const queryInterface = `
// Queryer database/sql compatible query interface
type Queryer interface {
	Exec(string, ...interface{}) (sql.Result, error)
	Query(string, ...interface{}) (*sql.Rows, error)
	QueryRow(string, ...interface{}) *sql.Row
}
`

const pgLoadColumnDef = `
SELECT
    a.attnum AS field_ordinal,
    a.attname AS column_name,
    format_type(a.atttypid, a.atttypmod) AS data_type,
    a.attnotnull AS not_null,
    COALESCE(pg_get_expr(ad.adbin, ad.adrelid), '') AS default_value,
    COALESCE(ct.contype = 'p', false) AS  is_primary_key,
    CASE
        WHEN a.atttypid = ANY ('{int,int8,int2}'::regtype[])
          AND EXISTS (
             SELECT 1 FROM pg_attrdef ad
             WHERE  ad.adrelid = a.attrelid
             AND    ad.adnum   = a.attnum
             AND    ad.adsrc = 'nextval('''
                || (pg_get_serial_sequence (a.attrelid::regclass::text
                                          , a.attname))::regclass
                || '''::regclass)'
             )
            THEN CASE a.atttypid
                    WHEN 'int'::regtype  THEN 'serial'
                    WHEN 'int8'::regtype THEN 'bigserial'
                    WHEN 'int2'::regtype THEN 'smallserial'
                 END
        WHEN a.atttypid = ANY ('{uuid}'::regtype[]) AND COALESCE(pg_get_expr(ad.adbin, ad.adrelid), '') != ''
            THEN 'autogenuuid'
        ELSE format_type(a.atttypid, a.atttypmod)
    END AS data_type,
    a.atttypid,
    t.typname,
    t.typlen
FROM pg_attribute a
JOIN ONLY pg_class c ON c.oid = a.attrelid
JOIN ONLY pg_type t ON t.oid = a.atttypid
JOIN ONLY pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_constraint ct ON ct.conrelid = c.oid
AND a.attnum = ANY(ct.conkey) AND ct.contype = 'p'
LEFT JOIN pg_attrdef ad ON ad.adrelid = c.oid AND ad.adnum = a.attnum
WHERE a.attisdropped = false
AND n.nspname = $1
AND c.relname = $2
AND a.attnum > 0
AND TRUE IS DISTINCT FROM ( pg_catalog.col_description(a.attrelid, a.attnum) ~ ANY ( $3::text[] ))
ORDER BY a.attnum;
`

const pgLoadTableDef = `
SELECT
c.relkind AS type,
c.relname AS table_name
FROM pg_class c
JOIN ONLY pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1
AND c.relkind = 'r'
ORDER BY c.relname
`

// TypeMap go/db type map struct
type TypeMap struct {
	DBTypes        []string `toml:"db_types"`
	NotNullGoType  string   `toml:"notnull_go_type"`
	NullableGoType string   `toml:"nullable_go_type"`
}

// AutoKeyMap auto generating key config
type AutoKeyMap struct {
	Types []string `toml:"db_types"`
}

// PgTypeMapConfig go/db type map struct toml config
type PgTypeMapConfig map[string]TypeMap

// PgTable postgres table
type PgTable struct {
	Schema      string
	Name        string
	DataType    string
	AutoGenPk   bool
	PrimaryKeys []*PgColumn
	Columns     []*PgColumn
}

var autoGenKeyCfg = &AutoKeyMap{
	Types: []string{"smallserial", "serial", "bigserial", "autogenuuid"},
}

func (t *PgTable) setPrimaryKeyInfo(cfg *AutoKeyMap) {
	t.AutoGenPk = false
	for _, c := range t.Columns {
		if c.IsPrimaryKey {
			t.PrimaryKeys = append(t.PrimaryKeys, c)
			for _, typ := range cfg.Types {
				if c.DDLType == typ {
					t.AutoGenPk = true
				}
			}
		}
	}
	return
}

// PgColumn postgres columns
type PgColumn struct {
	FieldOrdinal int
	Name         string
	DataType     string
	DDLType      string
	NotNull      bool
	DefaultValue sql.NullString
	IsPrimaryKey bool
	TypeOID      int64
	TypeName     string
	TypeLength   int16
}

// Struct go struct
type Struct struct {
	Name    string
	Table   *PgTable
	Comment string
	Fields  []*StructField
}

// StructTmpl go struct passed to template
type StructTmpl struct {
	Struct *Struct
}

// StructField go struct field
type StructField struct {
	Name     string
	Type     string
	BaseType string
	Tag      string
	Column   *PgColumn
}

// PgLoadTypeMapFromFile load type map from toml file
func PgLoadTypeMapFromFile(filePath string) (*PgTypeMapConfig, error) {
	var conf PgTypeMapConfig
	if _, err := toml.DecodeFile(filePath, &conf); err != nil {
		return nil, errors.Wrap(err, "faild to parse config file")
	}
	return &conf, nil
}

// PgLoadColumnDef load Postgres column definition
func PgLoadColumnDef(db Queryer, schema string, table string, exclusions []string) ([]*PgColumn, error) {
	colDefs, err := db.Query(pgLoadColumnDef, schema,table, pq.Array(exclusions))
	if err != nil {
		return nil, errors.Wrap(err, "failed to load table def")
	}

	cols := []*PgColumn{}
	for colDefs.Next() {
		c := &PgColumn{}
		err := colDefs.Scan(
			&c.FieldOrdinal,
			&c.Name,
			&c.DataType,
			&c.NotNull,
			&c.DefaultValue,
			&c.IsPrimaryKey,
			&c.DDLType,
			&c.TypeOID,
			&c.TypeName,
			&c.TypeLength,
		)
		if err != nil {
			return nil, errors.Wrap(err, "failed to scan")
		}
		cols = append(cols, c)
	}
	return cols, nil
}

// PgLoadTableDef load Postgres table definition
func PgLoadTableDef(db Queryer, schema string,exclusions []string) ([]*PgTable, error) {

	log.Infof("Finding tables in %s", schema)
	tbDefs, err := db.Query(pgLoadTableDef, schema)
	if err != nil {
		return nil, errors.Wrap(err, "failed to load table def")
	}
	tbs := []*PgTable{}
	for tbDefs.Next() {
		t := &PgTable{Schema: schema}
		err := tbDefs.Scan(
			&t.DataType,
			&t.Name,
		)
		if err != nil {
			return nil, errors.Wrap(err, "failed to scan")
		}

		log.Infof("Finding columns for %s", t.Name)
		cols, err := PgLoadColumnDef(db, schema, t.Name,exclusions)
		if err != nil {
			return nil, errors.Wrap(err, fmt.Sprintf("failed to get columns of %s", t.Name))
		}
		t.Columns = cols
		tbs = append(tbs, t)
	}
	return tbs, nil
}

func contains(v string, l []string) bool {

	if strings.HasPrefix(v, "character varying") {
		v = "character varying"
	} else if strings.HasPrefix(v, "numeric") {
		v = "numeric"
	} else if strings.HasPrefix(v, "character") {
		v = "character"
	}

	sort.Strings(l)
	i := sort.SearchStrings(l, v)
	if i < len(l) && l[i] == v {
		return true
	}

	return false
}

// PgConvertType converts type
func PgConvertType(col *PgColumn, typeCfg *PgTypeMapConfig) string {

	cfg := map[string]TypeMap(*typeCfg)
	typ := cfg["default"].NotNullGoType
	for _, v := range cfg {
		if contains(col.DataType, v.DBTypes) {
			if col.NotNull {
				return v.NotNullGoType
			}
			return v.NullableGoType
		}
	}
	return typ
}

// PgColToField converts pg column to go struct field
func PgColToField(col *PgColumn, typeCfg *PgTypeMapConfig) (*StructField, error) {
	stfType := PgConvertType(col, typeCfg)

	// grab the base type as well for mocking
	old := col.NotNull
	col.NotNull = true
	baseType := PgConvertType(col, typeCfg)
	col.NotNull = old

	stf := &StructField{
		Name:     varfmt.PublicVarName(col.Name),
		BaseType: baseType,
		Type:     stfType,
		Column:   col,
	}

	// as a hack, if the stf.Name is Memberid convert it to MemberID
	if stf.Name == "Memberid" {
		stf.Name = "MemberID"
	}

	return stf, nil
}

// PgTableToStruct converts table def to go struct
func PgTableToStruct(t *PgTable, typeCfg *PgTypeMapConfig, keyConfig *AutoKeyMap) (*Struct, error) {
	t.setPrimaryKeyInfo(keyConfig)
	s := &Struct{
		Name:  varfmt.PublicVarName(t.Name),
		Table: t,
	}
	var fs []*StructField
	for _, c := range t.Columns {
		f, err := PgColToField(c, typeCfg)
		if err != nil {
			return nil, errors.Wrap(err, "faield to convert col to field")
		}
		fs = append(fs, f)
	}
	s.Fields = fs
	return s, nil
}

// PgExecuteCustomTmpl execute custom template
func PgExecuteCustomTmpl(st *StructTmpl, customTmpl string) ([]byte, error) {
	var src []byte
	tpl, err := template.New("struct").Funcs(tmplFuncMap).Parse(customTmpl)
	if err != nil {
		return src, errors.Wrap(err, "failed to parse template")
	}
	buf := new(bytes.Buffer)
	if err := tpl.Execute(buf, st); err != nil {
		return src, errors.Wrap(err, fmt.Sprintf("failed to execute custom template:\n%s", src))
	}
	src, err = format.Source(buf.Bytes())
	if err != nil {
		return src, errors.Wrap(err, fmt.Sprintf("failed to format code:\n%s", src))
	}
	return src, nil
}

// PgCreateStruct creates struct from given schema
func PgCreateStruct(
	db Queryer, schema, typeMapPath, pkgName, customTmpl, importTmpl string, exTbls []string,exCmts []string) ([]byte, error) {
	var src []byte
	pkgDef := []byte(fmt.Sprintf("package %s\n\n", pkgName))
	src = append(src, pkgDef...)

	if importTmpl != "" {
		tmpl, err := ioutil.ReadFile(importTmpl)
		if err != nil {
			return nil, err
		}
		src = append(src, tmpl...)
	}

	tbls, err := PgLoadTableDef(db, schema, exCmts)
	if err != nil {
		return src, errors.Wrap(err, "faield to load table definitions")
	}
	cfg := &PgTypeMapConfig{}
	if _, err := toml.DecodeFile(typeMapPath, cfg); err != nil {
		return src, errors.Wrap(err, fmt.Sprintf("failed to decode type map file %s", typeMapPath))
	}
	for _, tbl := range tbls {
		if contains(tbl.Name, exTbls) {
			continue
		}
		st, err := PgTableToStruct(tbl, cfg, autoGenKeyCfg)
		if err != nil {
			return src, errors.Wrap(err, "faield to convert table definition to struct")
		}
		if customTmpl != "" {
			tmpl, err := ioutil.ReadFile(customTmpl)
			if err != nil {
				return nil, err
			}
			s, err := PgExecuteCustomTmpl(&StructTmpl{Struct: st}, string(tmpl))
			if err != nil {
				return nil, errors.Wrap(err, "PgExecuteCustomTmpl failed")
			}
			src = append(src, s...)
		}
	}
	return src, nil
}
