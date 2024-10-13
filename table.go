package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
)

const (
	TYPE_ERROR = 0
	TYPE_BYTES = 1
	TYPE_INT64 = 2
)

type DB struct {
	Path   string
	kv     KV
	tables map[string]*TableDef
}

type TableDef struct {
	Name     string
	Types    []uint32 //col type
	Cols     []string //col name
	Prefixes []uint32
	Indexes  [][]string
}

// table cell
type Value struct {
	Type uint32
	I64  int64
	Str  []byte
}

// represents a list of col names and values
type Record struct {
	Cols []string
	Vals []Value
}

func (rec *Record) AddStr(col string, val []byte) *Record {
	rec.Cols = append(rec.Cols, col)
	rec.Vals = append(rec.Vals, Value{Type: TYPE_BYTES, Str: val})

	return rec
}

func (rec *Record) AddInt64(col string, val int64) *Record {
	rec.Cols = append(rec.Cols, col)
	rec.Vals = append(rec.Vals, Value{Type: TYPE_INT64, I64: val})

	return rec
}
func (rec *Record) Get(key string) *Value {
	for i, c := range rec.Cols {
		if c == key {
			return &rec.Vals[i]
		}
	}

	return nil
}

// INTERNAL TABLES
// store metadata
var TDEF_META = &TableDef{
	Name:     "@meta",
	Types:    []uint32{TYPE_BYTES, TYPE_BYTES},
	Cols:     []string{"key", "val"},
	Prefixes: []uint32{1},
	Indexes:  [][]string{{"key"}},
}

// store table schemas
var TDEF_TABLE = &TableDef{
	Name:     "@table",
	Types:    []uint32{TYPE_BYTES, TYPE_BYTES},
	Cols:     []string{"name", "def"},
	Prefixes: []uint32{2},
	Indexes:  [][]string{{"name"}},
}

var INTERNAL_TABLES map[string]*TableDef = map[string]*TableDef{
	"@meta":  TDEF_META,
	"@table": TDEF_TABLE,
}

// reorder records to defined col. order
func reorderRecord(tdef *TableDef, rec Record) ([]Value, error) {
	assert(len(rec.Cols) == len(rec.Vals))
	out := make([]Value, len(tdef.Cols))
	for i, c := range tdef.Cols {
		v := rec.Get(c)
		if v == nil {
			continue
		}
		if v.Type != tdef.Types[i] {
			return nil, fmt.Errorf("bad column type: %s", c)
		}
		out[i] = *v
	}

	return out, nil
}

func valuesComplete(tdef *TableDef, vals []Value, n int) error {
	for i, v := range vals {
		if i < n && v.Type == 0 {
			return fmt.Errorf("missing column: %s", tdef.Cols[i])
		} else if i >= n && v.Type != 0 {
			return fmt.Errorf("extra column: %s", tdef.Cols[i])
		}
	}

	return nil
}

// escape null byte so string doesnt contain no null byte
func escapeString(in []byte) []byte {
	toEscape := bytes.Count(in, []byte{0}) + bytes.Count(in, []byte{1})
	if toEscape == 0 {
		return in
	}

	out := make([]byte, len(in)+toEscape)
	pos := 0
	for _, ch := range in {
		if ch <= 1 {
			out[pos+0] = 0x01
			out[pos+1] = ch + 1
			pos += 2
		} else {
			out[pos] = ch
			pos += 1
		}
	}
	return out
}

func unescapeString(in []byte) []byte {
	if bytes.Count(in, []byte{1}) == 0 {
		return in
	}

	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		if in[i] == 0x01 {
			// 01 01 -> 00
			i++
			assert(in[i] == 1 || in[i] == 2)
			out = append(out, in[i]-1)
		} else {
			out = append(out, in[i])
		}

	}

	return out
}

// order preserving encoding
func encodeValues(out []byte, vals []Value) []byte {
	for _, v := range vals {
		switch v.Type {
		case TYPE_INT64:
			var buf [8]byte
			u := uint64(v.I64) + (1 << 63)        // flip the sign bit
			binary.BigEndian.PutUint64(buf[:], u) // big endian
			out = append(out, buf[:]...)
		case TYPE_BYTES:
			out = append(out, escapeString(v.Str)...)
			out = append(out, 0) // null-terminated
		default:
			panic("what?")
		}
	}

	return out
}

func encodeKey(out []byte, prefix uint32, vals []Value) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], prefix)
	out = append(out, buf[:]...)
	out = encodeValues(out, vals)

	return out
}

func decodeKey(in []byte, out []Value) {
	decodeValues(in[4:], out)
}

func decodeValues(in []byte, out []Value) {
	for i := range out {
		switch out[i].Type {
		case TYPE_INT64:
			u := binary.BigEndian.Uint64(in[:8])
			out[i].I64 = int64(u - (1 << 63))
			in = in[8:]
		case TYPE_BYTES:
			idx := bytes.IndexByte(in, 0)
			assert(idx >= 0)
			out[i].Str = unescapeString(in[:idx])
			in = in[idx+1:]
		default:
			panic("what?")
		}
	}

	assert(len(in) == 0)
}

// check for missing columns
func checkRecord(tdef *TableDef, rec Record, n int) ([]Value, error) {
	vals, err := reorderRecord(tdef, rec)
	if err != nil {
		return nil, err
	}

	err = valuesComplete(tdef, vals, n)
	if err != nil {
		return nil, err
	}
	return vals, nil
}

// extract multiple col. values
func getValues(tdef *TableDef, rec Record, cols []string) ([]Value, error) {
	vals := make([]Value, len(cols))
	for i, c := range cols {
		v := rec.Get(c)
		if v == nil {
			return nil, fmt.Errorf("missing col.: %s", tdef.Cols[i])
		}

		if v.Type != tdef.Types[slices.Index(tdef.Cols, c)] {
			return nil, fmt.Errorf("bad column type: %s", c)
		}
		vals[i] = *v
	}
	return vals, nil
}

// get a single row by primary key
func dbGet(db *DB, tdef *TableDef, rec *Record) (bool, error) {
	vals, err := getValues(tdef, *rec, tdef.Indexes[0])
	if err != nil {
		return false, err
	}

	//scan operation
	sc := Scanner{
		Cmp1: CMP_GE,
		Cmp2: CMP_LE,
		Key1: Record{tdef.Indexes[0], vals},
		Key2: Record{tdef.Indexes[0], vals},
	}

	if err := dbScan(db, tdef, &sc); err != nil || !sc.Valid() {
		return false, err
	}
	sc.Deref(rec)
	return true, nil
}

func (db *DB) Get(table string, rec *Record) (bool, error) {
	tdef := getTableDef(db, table)
	if tdef == nil {
		return false, fmt.Errorf("table not found: %s", table)
	}

	return dbGet(db, tdef, rec)
}

const TABLE_PREFIX_MIN = 100

func tableDefCheck(tdef *TableDef) error {
	// very table schema
	bad := tdef.Name == "" || len(tdef.Cols) == 0
	bad = bad || len(tdef.Cols) != len(tdef.Types)
	bad = bad || !(1 <= tdef.PKeys && int(tdef.PKeys) <= len(tdef.Cols))
	if bad {
		return fmt.Errorf("bad table schema: %s", tdef.Name)
	}

	return nil
}

func (db *DB) TableNew(tdef *TableDef) error {
	if err := tableDefCheck(tdef); err != nil {
		return err
	}

	// check existing table
	table := (&Record{}).AddStr("name", []byte(tdef.Name))
	ok, err := dbGet(db, TDEF_TABLE, table)
	assert(err == nil)
	if ok {
		return fmt.Errorf("table exists: %s", tdef.Name)
	}

	// alllocate a new prefix
	assert(tdef.Prefix == 0)
	tdef.Prefix = TABLE_PREFIX_MIN
	meta := (&Record{}).AddStr("key", []byte("next_prefix"))
	ok, err = dbGet(db, TDEF_META, meta)
	assert(err == nil)
	if ok {
		tdef.Prefix = binary.LittleEndian.Uint32(meta.Get("val").Str)
		assert(tdef.Prefix > TABLE_PREFIX_MIN)
	} else {
		meta.AddStr("val", make([]byte, 4))
	}

	binary.LittleEndian.PutUint32(meta.Get("val").Str, tdef.Prefix+1)
	_, err = dbUpdate(db, TDEF_META, &DBUpdateReq{Record: *meta})
	if err != nil {
		return err
	}

	val, err := json.Marshal(tdef)
	assert(err == nil)
	table.AddStr("def", val)
	_, err = dbUpdate(db, TDEF_TABLE, &DBUpdateReq{Record: *table})

	return err
}

// get table schema by naem
func getTableDef(db *DB, name string) *TableDef {
	if tdef, ok := INTERNAL_TABLES[name]; ok {
		return tdef // expose internal tables
	}
	tdef := db.tables[name]
	if tdef == nil {
		if tdef = getTableDefDB(db, name); tdef != nil {
			db.tables[name] = tdef
		}
	}
	return tdef
}

func getTableDefDB(db *DB, name string) *TableDef {
	rec := (&Record{}).AddStr("name", []byte(name))
	ok, err := dbGet(db, TDEF_TABLE, rec)
	assert(err == nil)
	if !ok {
		return nil
	}

	tdef := &TableDef{}
	err = json.Unmarshal(rec.Get("def").Str, tdef)
	assert(err == nil)

	return tdef
}

type DBUpdateReq struct {
	Record  Record
	Mode    int
	Updated bool
	Added   bool
}

// add row to table
func dbUpdate(db *DB, tdef *TableDef, dbreq *DBUpdateReq) (bool, error) {
	values, err := checkRecord(tdef, dbreq.Record, len(tdef.Cols))
	if err != nil {
		return false, err
	}

	key := encodeKey(nil, tdef.Prefix, values[:tdef.PKeys])
	val := encodeValues(nil, values[tdef.PKeys:])
	req := UpdateReq{Key: key, Val: val, Mode: dbreq.Mode}
	if _, err := db.kv.Update(&req); err != nil {
		return false, err
	}

	dbreq.Added, dbreq.Updated = req.Added, req.Updated
	return req.Updated, err
}

// addin a record
func (db *DB) Set(table string, dbreq *DBUpdateReq) (bool, error) {
	tdef := getTableDef(db, table)
	if tdef == nil {
		return false, fmt.Errorf("table not found: %s", table)
	}

	return dbUpdate(db, tdef, dbreq)
}

func (db *DB) Insert(table string, rec Record) (bool, error) {
	return db.Set(table, &DBUpdateReq{Record: rec, Mode: MODE_INSERT_ONLY})
}
func (db *DB) Update(table string, rec Record) (bool, error) {
	return db.Set(table, &DBUpdateReq{Record: rec, Mode: MODE_UPDATE_ONLY})
}
func (db *DB) Upsert(table string, rec Record) (bool, error) {
	return db.Set(table, &DBUpdateReq{Record: rec, Mode: MODE_UPSERT})
}

// delete a record by primary key
func dbDelete(db *DB, tdef *TableDef, rec Record) (bool, error) {
	vals, err := checkRecord(tdef, rec, tdef.PKeys)
	if err != nil {
		return false, err
	}

	key := encodeKey(nil, tdef.Prefix, vals[:tdef.PKeys])
	return db.kv.Del(key)
}

func (db *DB) Delete(table string, rec Record) (bool, error) {
	tdef := getTableDef(db, table)
	if tdef == nil {
		return false, fmt.Errorf("table not found: %s", table)
	}

	return dbDelete(db, tdef, rec)
}

func (db *DB) Open() error {
	db.kv.Path = db.Path
	db.tables = map[string]*TableDef{}

	// opening kv store
	return db.kv.Open()
}

func (db *DB) Close() {
	db.kv.Close()
}

// scanner decodes KV's into rows
// iterator for range queries
// Scanner is a wrapper for B+ Tree iterator
type Scanner struct {
	Cmp1 int
	Cmp2 int

	// range from Key 1 to key2
	Key1 Record
	Key2 Record

	// internal
	tdef   *TableDef
	iter   *BIter
	keyEnd []byte
}

// within range or not
func (sc *Scanner) Valid() bool {
	if !sc.iter.Valid() {
		return false
	}

	key, _ := sc.iter.Deref()
	return cmpOk(key, sc.Cmp2, sc.keyEnd)
}

// movin underlying B+ tree iterator
func (sc *Scanner) Next() {
	assert(sc.Valid())
	if sc.Cmp1 > 0 {
		sc.iter.Next()
	} else {
		sc.iter.Prev()
	}
}

// return current row
func (sc *Scanner) Deref(rec *Record) {
	assert(sc.Valid())

	// fetch KV from iterator
	key, val := sc.iter.Deref()

	// decode KV into cols
	rec.Cols = sc.tdef.Cols
	rec.Vals = rec.Vals[:0]
	for _, type_ := range sc.tdef.Types {
		rec.Vals = append(rec.Vals, Value{Type: type_})
	}

	decodeKey(key, rec.Vals[:sc.tdef.PKeys])
	decodeValues(val, rec.Vals[sc.tdef.PKeys:])
}

func dbScan(db *DB, tdef *TableDef, req *Scanner) error {
	switch {
	case req.Cmp1 > 0 && req.Cmp2 < 0:
	case req.Cmp1 < 0 && req.Cmp2 > 0:
	default:
		return fmt.Errorf("bad range")
	}

	req.tdef = tdef

	// reorder input cols acc. to schema
	val1, err := checkRecord(tdef, req.Key1, tdef.PKeys)
	if err != nil {
		return err
	}
	val2, err := checkRecord(tdef, req.Key2, tdef.PKeys)
	if err != nil {
		return err
	}

	// encode primary key
	keyStart := encodeKey(nil, tdef.Prefix, val1[:tdef.PKeys])
	req.keyEnd = encodeKey(nil, tdef.Prefix, val2[:tdef.PKeys])

	// seek to start key
	req.iter = db.kv.tree.Seek(keyStart, req.Cmp1)
	return nil
}

func (db *DB) Scan(table string, req *Scanner) error {
	tdef := getTableDef(db, table)
	if tdef == nil {
		return fmt.Errorf("table not found: %s", table)
	}

	return dbScan(db, tdef, req)
}
