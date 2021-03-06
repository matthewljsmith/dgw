package main

import (
	"io"
	"log"
	"os"

	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	connStr = kingpin.Arg(
		"conn", "PostgreSQL connection string in URL format").Required().String()
	schema = kingpin.Flag(
		"schema", "PostgreSQL schema name").Default("public").Short('s').String()
	pkgName          = kingpin.Flag("package", "package name").Default("main").Short('p').String()
	typeMapFilePath  = kingpin.Flag("typemap", "column type and go type map file path").Short('t').String()
	exTbls           = kingpin.Flag("exclude", "table names to exclude").Short('x').Strings()
	importTmpl       = kingpin.Flag("imports", "custom import template path").String()
	customTmpl       = kingpin.Flag("template", "custom template path").String()
	outFile          = kingpin.Flag("output", "output file path").Short('o').String()
	noQueryInterface = kingpin.Flag("no-interface", "output without Queryer interface").Bool()
	exCmts           = kingpin.Flag("not-comment", "exclude columns with a comment (description) that matches this regex").Strings()
)

func main() {
	kingpin.Parse()

	conn, err := OpenDB(*connStr)
	if err != nil {
		log.Fatal(err)
	}

	st, err := PgCreateStruct(conn, *schema, *typeMapFilePath, *pkgName, *customTmpl, *importTmpl, *exTbls, *exCmts)
	if err != nil {
		log.Fatal(err)
	}

	var src []byte
	if *noQueryInterface {
		src = st
	} else {
		q := []byte(queryInterface)
		src = append(st, q...)
	}

	var out io.Writer
	if *outFile != "" {
		out, err = os.Create(*outFile)
		if err != nil {
			log.Fatalf("failed to create output file %s: %s", *outFile, err)
		}
	} else {
		out = os.Stdout
	}
	if _, err := out.Write(src); err != nil {
		log.Fatal(err)
	}
}
