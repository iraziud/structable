/* Structable is a struct-to-table mapper for databases.

THE INTERFACES DEFINED HERE MAY BE VOLATILE UNTIL MAY, 2014.

Structable is not quite a struct-relational mapper. Instead of attempting to
manage all aspects of relational mapping, it provides a basic CRUD layer for
mapping structs to table rows in an existing schema.

Importantly, Structable does not do any relation management. There is no
magic to convert structs, arrays, or maps to references to other tables.
(If you want that, you may prefer GORM or GORP)

Structable uses Squirrel for statement building, and you may also use
Squirrel for working with your data.

Basic Usage:

	// Implement a Record. Records use annotations to describe which
	// struct fields are related to which database columns
	type Foo struct {
		Id int64 `stbl:"id,PRIMARY_KEY,AUTO_INCREMENT"`
		Name string `stbl:"name"`
		SomethingElse string // This has no tag, so is ingnored.
	}

	// Use it:
	func main() {

		// Create our foo Record.
		foo := new(Foo)
		foo.Name = "Hi"

		// Get a handle to a database/sql.Db, a squirrel.StmtCache, or anything else
		// that implements the DB's interface
		db := getMyDbHandle()

		// Create a recorder that is bound to our new struct. Not that we associate
		// this to a database table (my_table) here, too.
		recorder := structable.New(db, "mysql").Bind("my_table", foo)

		// At this point, the recorder is mutating our Foo instance. Operations on
		// the recorder will change `foo`.

		// Now foo.Id will be set, since it is an AUTO_INCREMENT key.
		println(foo.Id)

		// Update
		foo.Name = "Hello"
		recorder.Update()

		// Load it (for changes, etc)
		recorder.Load()

		// Check to see if foo exists. (Check to see if Foo's PRIMARY_KEY field(s) exist)
		recorder.Exists()

		// Delete foo
		recorder.Delete()

	}


The intended usage pattern differs a little from the above. The original idea
was to have the record kept inside of an active record. This tightly couples the
record and the recorder. See the unit tests for an example.

The `stbl` tag is of the form:

	stbl:"field_name [,PRIMARY_KEY[,AUTO_INCREMENT]]"

The field name is passed verbatim to the database. So `fieldName` will go to the database as `fieldName`.
Structable is not at all opinionated about how you name your tables or fields. Some databases are, though, so
you may need to be careful about your own naming conventions.

`PRIMARY_KEY` tells Structable that this field is (one of the pieces of) the primary key. Aliases: 'PRIMARY KEY'

`AUTO_INCREMENT` tells Structable that this field is created by the database, and should never
be assigned during an Insert(). Aliases: SERIAL, AUTO INCREMENT

Things Structable doesn't do (by design)

	- Guess table or column names. You must specify these.
	- Handle relations between tables.
	- Manage the schema.
	- Transform complex struct fields into simple ones (that is, serialize fields).


*/
package structable

import (
	"github.com/lann/squirrel"
	"reflect"
	"strings"
	"fmt"
	"database/sql"
)

const StructableTag = "stbl"

/* Record describes a struct that can be stored.

Example:
	type Stool struct {
		Id 		 int 	`stbl:"id PRIMARY_KEY AUTO_INCREMENT"`
		Legs 	 int    `stbl:"number_of_legs"`
		Material string `stbl:"material"`
		Ignored  string // will not be stored.
	}


*/
type Record interface {}

// Internal representation of a field on a database table, and its
// relation to a struct field.
type field struct {
	// name = Struct field name
	// column = table column name
	name, column string
	// Is a primary key
	isKey bool
	// Is an auto increment
	isAuto bool
}

// A Recorder is responsible for managing the persistence of a Record.
// A Recorder is bound to a struct, which it then examines for fields
// that should be stored in the database. From that point on, a recorder
// can manage the persistent lifecycle of the record.
type Recorder interface {
	// Bind this Recorder to a table and to a Record.
	//
	// The table name is used verbatim. DO NOT TRUST USER-SUPPLIED VALUES.
	//
	// The struct is examined for tags, and those tags are parsed and used to determine
	// details about each field.
	Bind(string, Record) Recorder
	// Insert inserts the bound Record into the bound table.
	Insert() error

	// Update updates all of the fields on the bound Record based on the PRIMARY_KEY fields.
	//
	// Essentially, it does something like this:
	// 	UPDATE bound_table SET every=?, field=?, but=?, keys=? WHERE primary_key=?
	Update() error

	// Deletes a Record based on its PRIMARY_KEY(s).
	Delete() error

	// Checks to see if a Record exists in the bound table by checking for the presence of the PRIMARY_KEY(s).
	Exists() (bool, error)

	// Loads the entire Record using the value of the PRIMARY_KEY(s)
	// This will only fetch columns that are mapped on the bound Record. But you can think of it
	// as doing something like this:
	//
	// 	SELECT * FROM bound_table WHERE id=? LIMIT 1
	//
	// And then mapping the result to the currently bound Record.
	Load() error

	// This returns the column names used for the primary key.
	//Key() []string
}

// Implements the Recorder interface, and stores data in a DB.
type DbRecorder struct {
	builder *squirrel.StatementBuilderType
	db squirrel.DBProxyBeginner
	table string
	fields []*field
	key []*field
	record Record
	flavor string
}

// New creates a new DbRecorder.
//
// (The squirrel.DBProxy interface defines the functions normal for a database connection
// or a prepared statement cache.)
func New(db squirrel.DBProxyBeginner, flavor string) *DbRecorder {
	b := squirrel.StatementBuilder.RunWith(db)
	r := new(DbRecorder)
	r.builder = &b
	r.db = db
	r.flavor = flavor

	if flavor == "postgres" {
		b = b.PlaceholderFormat(squirrel.Dollar)
	}

	return r
}


// NewFromBuilder creates a new DbRecorder with an existing squirrel.StatementBuilderType.
/*
func NewFromBuilder(builder *squirrel.StatementBuilderType) *DbRecorder {
	r := new(DbRecorder)
	r.builder = builder

	return r
}
*/

// Bind binds this particular instance to a particular record.
//
// This takes a given structable.Record and binds it to the recorder. That means
// that the recorder will track all changes to the Record.
//
// The table name tells the recorder which database table to link this record
// to. All storage operations will use that table.
func (s *DbRecorder) Bind(tableName string, ar Record) Recorder {

	// "To be is to be the value of a bound variable." - W. O. Quine

	// Get the table name
	s.table = tableName

	// Get the fields
	s.scanFields(ar)

	s.record = ar

	return Recorder(s)
}

// Key gets the string names of the fields used as primary key.
func (s *DbRecorder) Key() []string {
	key := make([]string, len(s.key))

	for i, f := range s.key {
		key[i] = f.column
	}

	return key
}

// Load selects the record from the database and loads the values into the bound Record.
//
// This modifies the Record in-place.
func (s *DbRecorder) Load() error {
	whereParts := s.whereIds()
	dest := s.fieldReferences(false)

	q := s.builder.Select(s.colList(false)...).From(s.table).Where(whereParts)
	err := q.QueryRow().Scan(dest...)

	return err
}

// Exists returns `true` if and only if there is at least one record that matches the primary keys for this Record.
//
// If the primary key on the Record has no value, this will look for records with no value (or the default
// value).
func (s *DbRecorder) Exists() (bool, error) {
	has := false
	whereParts := s.whereIds()

	q := s.builder.Select("COUNT(*) > 0").From(s.table).Where(whereParts)
	err := q.QueryRow().Scan(&has)

	return has, err
}

// Delete deletes the record from the underlying table.
//
// The fields on the present record will remain set, but not saved in the database.
func (s *DbRecorder) Delete() error {
	wheres := s.whereIds()
	q := s.builder.Delete(s.table).Where(wheres)
	_, err := q.Exec()
	return err
}

// Insert puts a new record into the database.
//
// This operation is particularly sensitive to DB differences in cases where AUTO_INCREMENT is set
// on a member of the Record.
func (s *DbRecorder) Insert() error {
	switch s.flavor {
	case "postgres":
		return s.insertPg()
	default:
		return s.insertStd()
	}
}

// Insert and assume that LastInsertId() returns something.
func (s *DbRecorder) insertStd() error {
	cols, vals := s.insertFields()
	q := s.builder.Insert(s.table).Columns(cols...).Values(vals...)

	ret, err := q.Exec()
	if err != nil {
		return err
	}

	for _, f := range s.fields {
		if f.isAuto {
			ar := reflect.Indirect(reflect.ValueOf(s.record))
			field := ar.FieldByName(f.name)

			id, err := ret.LastInsertId()
			if err != nil {
				return fmt.Errorf("Could not get last insert ID. Did you set the db flavor? %s", err)
				
			}

			if !field.CanSet() {
				return fmt.Errorf("Could not set %s to returned value", f.name)
			}
			field.SetInt(id)
		}
			
	
	}

	return err
}

// This implements the squirrel.Runner interface correctly.
type txRunner struct {
	*sql.Tx
}

func (t txRunner) QueryRow(s string, v ...interface{}) squirrel.RowScanner {
	return t.Tx.QueryRow(s, v)
}



// Postgres-specific insert.
func (s *DbRecorder) insertPg() error {
	cols, vals := s.insertFields()

	txr, err := s.db.Begin()
	if err != nil {
		return err
	}

	// Satisfy the squirrel.Runner interface.
	tx := txRunner{txr} //squirrel.NewStmtCacher(txr)

	q := s.builder.Insert(s.table).Columns(cols...).Values(vals...)
	//qq, aa, _ := q.ToSql()
	//fmt.Printf("QQ:%s, args %d\n", qq, len(aa))


	// Rollback unless Commit is called first.
	defer txr.Rollback()

	
	if _, err := q.RunWith(tx).Exec(); err != nil {
		return err
	}

	var lastid int64 = 0
	for _, f := range s.fields {
		if f.isAuto {
			ar := reflect.Indirect(reflect.ValueOf(s.record))
			field := ar.FieldByName(f.name)

			if !field.CanSet() {
				return fmt.Errorf("Could not set %s to returned value", f.name)
			}

			// Lazily grab id the first time we need it.
			if lastid == 0 {
				ss, _, _ := s.builder.Select("lastval()").ToSql()
				err := txr.QueryRow(ss).Scan(&lastid)
				//fmt.Printf("Last ID: %d\n", lastid)
				if err != nil {
					return err
				}
			}

			field.SetInt(lastid)
		}
	}
	txr.Commit()

	return nil
}

// Update update the values on an existing entry.
//
// This updates records where the Record's primary keys match the record in the database.
//
// If no entry is found, update will NOT create a new record.
func (s *DbRecorder) Update() error {

	whereParts := s.whereIds()
	updates := s.updateFields()
	q := s.builder.Update(s.table).SetMap(updates).Where(whereParts)

	_, err := q.Exec()
	return err
}

// colList gets a list of column names. If withKeys is false, columns that are
// designated as primary keys will not be returned in this list.
func (s *DbRecorder) colList(withKeys bool) []string {
	names := make([]string, 0, len(s.fields))

	for _, f := range s.fields {
		if !withKeys && f.isKey {
			continue
		}
		names = append(names, f.column)
	}

	return names
}

func (s *DbRecorder) fieldReferences(withKeys bool) []interface{} {
	refs := make([]interface{}, 0, len(s.fields))

	ar := reflect.Indirect(reflect.ValueOf(s.record))
	for _, f := range s.fields {
		if !withKeys && f.isKey {
			continue
		}

		ref := reflect.Indirect(ar.FieldByName(f.name))
		//ref := ar.FieldByName(f.name)
		if ref.IsValid() {
			refs = append(refs, ref.Addr().Interface())
		} else { // Should never hit this part.
			var skip interface{}
			refs = append(refs, &skip)
		}

	}

	return refs 
}

func (s *DbRecorder) insertFields() (columns []string, values []interface{}) {
	ar := reflect.Indirect(reflect.ValueOf(s.record))

	for _, field := range s.fields {
		// Serial fields are automatically set, so we don't everride, lest
		// we an invalid/duplicate key value.
		if field.isAuto {
			continue
		}

		// Get the value of the field we are going to store.
		//v := reflect.Indirect(reflect.ValueOf(ar.FieldByName(field.name))).Interface()
		v := ar.FieldByName(field.name).Interface()
		
		columns = append(columns, field.column)
		values = append(values, v)
	}

	return
}

// Produce fields to go into SetMap for an update.
// This will NOT update PRIMARY_KEY fields.
func (s *DbRecorder) updateFields() map[string]interface{} {
	ar := reflect.Indirect(reflect.ValueOf(s.record))
	update := make(map[string]interface{}, ar.NumField())

	for _, field := range s.fields {
		if field.isKey {
			continue
		}
		update[field.column] = ar.FieldByName(field.name).Interface()
	}

	return update
}

// whereIds gets a list of names and a list of values for all columns marked as primary
// keys.
func (s *DbRecorder) whereIds() map[string]interface{} { // ([]string, []interface{}) {
	clause := make(map[string]interface{}, len(s.key))

	ar := reflect.Indirect(reflect.ValueOf(s.record))

	for _, f := range s.key {
		clause[f.column] = ar.FieldByName(f.name).Interface()
		//fmt.Printf("Where parts: %V", clause[f.column])
	}


	return clause
}

// scanFields extracts the tags from all of the fields on a struct.
func (s *DbRecorder) scanFields(ar Record) {
	v := reflect.Indirect(reflect.ValueOf(ar))
	t := v.Type()
	count := t.NumField()
	keys := make([]*field, 0, 2)

	for i := 0; i < count; i++ {
		f := t.Field(i)
		// Skip fields with no tag.
		if len(f.Tag) == 0 {
			continue
		}
		sqtag := f.Tag.Get("stbl")
		if len(sqtag) == 0 {
			continue
		}

		parts := s.parseTag(f.Name, sqtag)
		field := new(field)
		field.name = f.Name
		field.column = parts[0]
		for _, part := range parts[1:] {
			part = strings.TrimSpace(part)
			switch part {
			case "PRIMARY_KEY", "PRIMARY KEY":
				field.isKey = true
				keys = append(keys, field)
			case "AUTO_INCREMENT", "SERIAL", "AUTO INCREMENT":
				field.isAuto = true
			}
		}
		s.fields = append(s.fields, field)
		s.key = keys
	}
	
}

// Parse the contents of a stbl tag.
func (s *DbRecorder) parseTag(fieldName, tag string) []string {
	parts := strings.Split(tag, ",")
	if len(parts) == 0 {
		return []string{fieldName}
	}
	return parts
}

