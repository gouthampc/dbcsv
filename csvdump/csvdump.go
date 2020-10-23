// Copyright 2020 Tamás Gulácsi.
//
//
// SPDX-License-Identifier: UPL-1.0 OR Apache-2.0

// Package main in csvdump represents a cursor->csv dumper
package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/text/encoding"

	"github.com/UNO-SOFT/dbcsv"
	"github.com/UNO-SOFT/spreadsheet"
	"github.com/UNO-SOFT/spreadsheet/ods"
	"github.com/UNO-SOFT/spreadsheet/xlsx"
	"github.com/godror/godror"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

func main() {
	if err := Main(); err != nil {
		log.Fatalf("%+v", err)
	}
}

func Main() error {
	flagConnect := flag.String("connect", os.Getenv("DB_ID"), "user/passw@sid to connect to")
	flagDateFormat := flag.String("date", dateFormat, "date format, in Go notation")
	flagSep := flag.String("sep", ";", "separator")
	flagHeader := flag.Bool("header", true, "print header")
	flagEnc := flag.String("encoding", dbcsv.DefaultEncoding.Name, "encoding to use for output")
	flagOut := flag.String("o", "-", "output (defaults to stdout)")
	flagRaw := flag.Bool("raw", false, "not real csv, just dump the raw data")
	flagSheets := dbcsv.FlagStrings()
	flag.Var(flagSheets, "sheet", "each -sheet=name:SELECT will become a separate sheet on the output ods")
	flagVerbose := flag.Bool("v", false, "verbose logging")
	flagCompress := flag.String("compress", "", "compress output with gz/gzip or zst/zstd/zstandard")
	flagCall := flag.Bool("call", false, "the first argument is not the WHERE, but the PL/SQL block to be called, the followings are not the columns but the arguments")

	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), strings.Replace(`Usage of {{.prog}}:
	{{.prog}} [options] 'T_able' 'F_ield=1'

will execute a "SELECT * FROM T_able WHERE F_ield=1" and dump all the columns;

	{{.prog}} -call [options] 'DB_lista.csv' 'p_a=1' 'p_b=c'

will execute "BEGIN :1 := DB_lista.csv(p_a=>:2, p_b=>3); END" with p_a=1, p_b=c
and dump all the columns of the cursor returned by the function.

`, "{{.prog}}", os.Args[0], -1))
		flag.PrintDefaults()
	}
	if *flagConnect == "" {
		*flagConnect = os.Getenv("BRUNO_ID")
	}
	flag.Parse()

	Log := func(...interface{}) error { return nil }
	if *flagVerbose {
		Log = func(keyvals ...interface{}) error {
			if len(keyvals)%2 != 0 {
				keyvals = append(keyvals, "")
			}
			vv := make([]interface{}, len(keyvals)/2)
			for i := range vv {
				v := fmt.Sprintf("%+v", keyvals[(i<<1)+1])
				if strings.Contains(v, " ") {
					v = `"` + v + `"`
				}
				vv[i] = fmt.Sprintf("%s=%s", keyvals[(i<<1)], v)
			}
			log.Println(vv...)
			return nil
		}
	}

	enc, err := dbcsv.EncFromName(*flagEnc)
	if err != nil {
		return err
	}
	dateFormat = *flagDateFormat
	dEnd = `"` + strings.NewReplacer(
		"2006", "9999",
		"01", "12",
		"02", "31",
		"15", "23",
		"04", "59",
		"05", "59",
	).Replace(dateFormat) + `"`

	var queries []string
	var params []interface{}
	if len(flagSheets.Strings) != 0 {
		queries = flagSheets.Strings
	} else if *flagCall {
		var buf strings.Builder
		fmt.Fprintf(&buf, `BEGIN :1 := %s(`, flag.Arg(0))
		params = make([]interface{}, flag.NArg()-1)
		for i, x := range flag.Args()[1:] {
			arg := strings.SplitN(x, "=", 2)
			params[i] = ""
			if len(arg) > 1 {
				params[i] = arg[1]
			}
			if i != 0 {
				buf.WriteString(", ")
			}
			fmt.Fprintf(&buf, "%s=>:%d", arg[0], i+2)
		}
		buf.WriteString("); END;")
		qry := buf.String()
		if Log != nil {
			Log("call", qry, "params", params)
		}
		queries = append(queries, qry)
	} else {
		var (
			where   string
			columns []string
		)
		if flag.NArg() > 1 {
			where = flag.Arg(1)
			if flag.NArg() > 2 {
				columns = flag.Args()[2:]
			}
		}
		qry := getQuery(flag.Arg(0), where, columns, dbcsv.DefaultEncoding)
		queries = append(queries, qry)
	}
	db, err := sql.Open("godror", *flagConnect)
	if err != nil {
		return fmt.Errorf("%s: %w", *flagConnect, err)
	}
	defer db.Close()
	ctx, cancel := dbcsv.Wrap(context.Background())
	defer cancel()

	fh := os.Stdout
	if !(*flagOut == "" || *flagOut == "-") {
		os.MkdirAll(filepath.Dir(*flagOut), 0775)
		if fh, err = os.Create(*flagOut); err != nil {
			return fmt.Errorf("%s: %w", *flagOut, err)
		}
	}
	defer fh.Close()
	wfh := io.WriteCloser(fh)
	if *flagCompress != "" {
		switch (strings.TrimSpace(strings.ToLower(*flagCompress)) + "  ")[:2] {
		case "gz":
			wfh = gzip.NewWriter(fh)
		case "zs":
			var err error
			if wfh, err = zstd.NewWriter(fh); err != nil {
				return err
			}
		}
	}

	if Log != nil {
		Log("msg", "writing", "file", fh.Name(), "encoding", enc)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		log.Printf("[WARN] Read-Only transaction: %v", err)
		if tx, err = db.BeginTx(ctx, nil); err != nil {
			return fmt.Errorf("%s: %w", "beginTx", err)
		}
	}
	defer tx.Rollback()

	if len(flagSheets.Strings) == 0 {
		w := io.Writer(encoding.ReplaceUnsupported(enc.NewEncoder()).Writer(wfh))
		if Log != nil {
			Log("env_encoding", dbcsv.DefaultEncoding.Name)
		}

		rows, columns, qErr := doQuery(ctx, tx, queries[0], params, *flagCall)
		if qErr != nil {
			err = qErr
		} else {
			defer rows.Close()
			err = dumpCSV(ctx, w, rows, columns, *flagHeader, *flagSep, *flagRaw, Log)
		}
	} else {
		var w spreadsheet.Writer
		if strings.HasSuffix(fh.Name(), ".xlsx") {
			w = xlsx.NewWriter(wfh)
		} else {
			w, err = ods.NewWriter(wfh)
			if err != nil {
				return err
			}
		}
		defer w.Close()
		dec := enc.Encoding.NewDecoder()
		var grp errgroup.Group
		for sheetNo := range queries {
			qry := queries[sheetNo]
			if qry, err = dec.String(qry); err != nil {
				return fmt.Errorf("%q: %w", queries[sheetNo], err)
			}
			var name string
			i := strings.IndexByte(qry, ':')
			if i >= 0 {
				name, qry = qry[:i], qry[i+1:]
			}
			if name == "" {
				name = strconv.Itoa(sheetNo + 1)
			}
			rows, columns, qErr := doQuery(ctx, tx, qry, nil, false)
			if qErr != nil {
				err = qErr
				break
			}
			header := make([]spreadsheet.Column, len(columns))
			if *flagHeader {
				for i, c := range columns {
					header[i].Name = c.Name
				}
			}
			sheet, sErr := w.NewSheet(name, header)
			if sErr != nil {
				rows.Close()
				err = sErr
				break
			}
			grp.Go(func() error {
				Log(name, qry)
				err := dumpSheet(ctx, sheet, rows, columns, Log)
				rows.Close()
				if closeErr := sheet.Close(); closeErr != nil && err == nil {
					return closeErr
				}
				return err
			})
		}
		if err != nil {
			return err
		}
		err = grp.Wait()
		if closeErr := w.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	cancel()
	if wfh != fh {
		if closeErr := wfh.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	if closeErr := fh.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	return err
}

func getQuery(table, where string, columns []string, enc encoding.Encoding) string {
	if table == "" && where == "" && len(columns) == 0 {
		if enc == nil {
			enc = encoding.Nop
		}
		b, err := ioutil.ReadAll(enc.NewDecoder().Reader(os.Stdin))
		if err != nil {
			panic(err)
		}
		return string(b)
	}
	table = strings.TrimSpace(table)
	if len(table) > 6 && strings.HasPrefix(strings.ToUpper(table), "SELECT ") {
		return table
	}
	cols := "*"
	if len(columns) > 0 {
		cols = strings.Join(columns, ", ")
	}
	if where == "" {
		return "SELECT " + cols + " FROM " + table //nolint:gas
	}
	return "SELECT " + cols + " FROM " + table + " WHERE " + where //nolint:gas
}

type queryer interface {
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

type execer interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}
type queryExecer interface {
	queryer
	execer
}

func doQuery(ctx context.Context, db queryExecer, qry string, params []interface{}, isCall bool) (*sql.Rows, []Column, error) {
	var rows *sql.Rows
	var err error
	const batchSize = 1024
	if !isCall {
		rows, err = db.QueryContext(ctx, qry, godror.FetchRowCount(batchSize), godror.PrefetchCount(batchSize))
	} else {
		var dRows driver.Rows
		params = append(append(make([]interface{}, 0, 2+len(params)),
			sql.Out{Dest: &dRows}, godror.FetchRowCount(batchSize), godror.PrefetchCount(batchSize)),
			params...)
		if _, err = db.ExecContext(ctx, qry, params...); err == nil {
			rows, err = godror.WrapRows(ctx, db, dRows)
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%q: %w", qry, err)
	}
	columns, err := getColumns(rows)
	if err != nil {
		rows.Close()
		return nil, nil, err
	}
	return rows, columns, nil
}

func dumpCSV(ctx context.Context, w io.Writer, rows *sql.Rows, columns []Column, header bool, sep string, raw bool, Log func(...interface{}) error) error {
	sepB := []byte(sep)
	dest := make([]interface{}, len(columns))
	bw := bufio.NewWriterSize(w, 65536)
	defer bw.Flush()
	values := make([]stringer, len(columns))
	for i, col := range columns {
		c := col.Converter(sep)
		values[i] = c
		dest[i] = c.Pointer()
	}
	if header && !raw {
		for i, col := range columns {
			if i > 0 {
				bw.Write(sepB)
			}
			csvQuote(bw, sep, col.Name)
		}
		bw.Write([]byte{'\n'})
	}

	start := time.Now()
	n := 0
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return fmt.Errorf("scan into %#v: %w", dest, err)
		}
		if raw {
			for i, data := range dest {
				if data == nil {
					continue
				}
				if sr, ok := values[i].(interface{ StringRaw() string }); ok {
					bw.WriteString(sr.StringRaw())
				} else {
					bw.WriteString(values[i].String())
				}
			}
		} else {
			for i, data := range dest {
				if i > 0 {
					bw.Write(sepB)
				}
				if data == nil {
					continue
				}
				bw.WriteString(values[i].String())
			}
		}
		bw.Write([]byte{'\n'})
		n++
	}
	err := rows.Err()
	dur := time.Since(start)
	if Log != nil {
		Log("msg", "dump finished", "rows", n, "dur", dur, "speed", float64(n)/float64(dur)*float64(time.Second), "error", err)
	}
	return err
}

func dumpSheet(ctx context.Context, sheet spreadsheet.Sheet, rows *sql.Rows, columns []Column, Log func(...interface{}) error) error {
	dest := make([]interface{}, len(columns))
	vals := make([]interface{}, len(columns))
	values := make([]stringer, len(columns))
	for i, col := range columns {
		c := col.Converter("")
		values[i] = c
		vals[i] = c
		dest[i] = c.Pointer()
	}
	start := time.Now()
	n := 0
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return fmt.Errorf("scan into %#v: %w", dest, err)
		}
		if err := sheet.AppendRow(vals...); err != nil {
			return err
		}
		n++
	}
	err := rows.Err()
	dur := time.Since(start)
	if Log != nil {
		Log("msg", "dump finished", "rows", n, "dur", dur, "speed", float64(n)/float64(dur)*float64(time.Second), "error", err)
	}
	return err
}

type Column struct {
	Name string
	reflect.Type
}

func (col Column) Converter(sep string) stringer {
	return getColConverter(col.Type, sep)
}

type stringer interface {
	String() string
	Pointer() interface{}
	Scan(interface{}) error
}

type ValString struct {
	Sep   string
	Value sql.NullString
}

func (v ValString) String() string            { return csvQuoteString(v.Sep, v.Value.String) }
func (v ValString) StringRaw() string         { return v.Value.String }
func (v *ValString) Pointer() interface{}     { return &v.Value }
func (v *ValString) Scan(x interface{}) error { return v.Value.Scan(x) }

type ValInt struct {
	Value sql.NullInt64
}

func (v ValInt) String() string {
	if v.Value.Valid {
		return strconv.FormatInt(v.Value.Int64, 10)
	}
	return ""
}
func (v *ValInt) Pointer() interface{}     { return &v.Value }
func (v *ValInt) Scan(x interface{}) error { return v.Value.Scan(x) }

type ValFloat struct {
	Value sql.NullFloat64
}

func (v ValFloat) String() string {
	if v.Value.Valid {
		return strconv.FormatFloat(v.Value.Float64, 'f', -1, 64)
	}
	return ""
}
func (v *ValFloat) Pointer() interface{}     { return &v.Value }
func (v *ValFloat) Scan(x interface{}) error { return v.Value.Scan(x) }

type ValTime struct {
	Value sql.NullTime
	Quote bool
}

var (
	dEnd       string
	dateFormat = "2006-01-02"
)

func (v ValTime) String() string {
	if !v.Value.Valid || v.Value.Time.IsZero() {
		return ""
	}
	if v.Value.Time.Year() < 0 {
		return dEnd
	}
	if v.Quote {
		return `"` + v.Value.Time.Format(dateFormat) + `"`
	}
	return v.Value.Time.Format(dateFormat)
}
func (v ValTime) StringRaw() string {
	if !v.Value.Valid || v.Value.Time.IsZero() {
		return ""
	}
	if v.Value.Time.Year() < 0 {
		return dEnd
	}
	return v.Value.Time.Format(dateFormat)
}

func (vt ValTime) ConvertValue(v interface{}) (driver.Value, error) {
	if v == nil {
		return sql.NullTime{}, nil
	}
	switch v := v.(type) {
	case sql.NullTime:
		return v, nil
	case time.Time:
		return sql.NullTime{Valid: !v.IsZero(), Time: v}, nil
	}
	return nil, fmt.Errorf("unknown value %T", v)
}
func (vt *ValTime) Scan(v interface{}) error {
	if v == nil {
		vt.Value = sql.NullTime{}
		return nil
	}
	switch v := v.(type) {
	case sql.NullTime:
		vt.Value = v
	case time.Time:
		vt.Value = sql.NullTime{Valid: !v.IsZero(), Time: v}
	default:
		return fmt.Errorf("unknown scan source %T", v)
	}
	return nil
}
func (v *ValTime) Pointer() interface{} { return v }

var typeOfTime, typeOfNullTime = reflect.TypeOf(time.Time{}), reflect.TypeOf(sql.NullTime{})

func getColConverter(typ reflect.Type, sep string) stringer {
	switch typ.Kind() {
	case reflect.String:
		return &ValString{Sep: sep}
	case reflect.Float32, reflect.Float64:
		return &ValFloat{}
	case reflect.Int32, reflect.Int64, reflect.Int:
		return &ValInt{}
	}
	switch typ {
	case typeOfTime, typeOfNullTime:
		return &ValTime{Quote: sep != "" && strings.Contains(dateFormat, sep)}
	}
	return &ValString{Sep: sep}
}

var bufPool = sync.Pool{New: func() interface{} { return bytes.NewBuffer(make([]byte, 0, 1024)) }}

func csvQuoteString(sep, s string) string {
	if sep == "" {
		return s
	}
	buf := bufPool.Get().(*bytes.Buffer)
	defer bufPool.Put(buf)
	buf.Reset()
	csvQuote(buf, sep, s)
	return buf.String()
}

func csvQuote(w io.Writer, sep, s string) (int, error) {
	needQuote := strings.Contains(s, sep) || strings.ContainsAny(s, `"`+"\n")
	if !needQuote {
		return io.WriteString(w, s)
	}
	n, err := w.Write([]byte{'"'})
	if err != nil {
		return n, err
	}
	m, err := io.WriteString(w, strings.Replace(s, `"`, `""`, -1))
	n += m
	if err != nil {
		return n, err
	}
	m, err = w.Write([]byte{'"'})
	return n + m, err
}

func getColumns(rows interface{}) ([]Column, error) {
	if r, ok := rows.(*sql.Rows); ok {
		types, err := r.ColumnTypes()
		if err != nil {
			return nil, err
		}
		cols := make([]Column, len(types))
		for i, t := range types {
			cols[i] = Column{Name: t.Name(), Type: t.ScanType()}
		}
		return cols, nil
	}

	colNames := rows.(driver.Rows).Columns()
	cols := make([]Column, len(colNames))
	r := rows.(driver.RowsColumnTypeScanType)
	for i, name := range colNames {
		cols[i] = Column{
			Name: name,
			Type: r.ColumnTypeScanType(i),
		}
	}
	return cols, nil
}

// vim: se noet fileencoding=utf-8:
