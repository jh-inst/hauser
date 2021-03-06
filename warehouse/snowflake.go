package warehouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"time"

	"github.com/fullstorydev/hauser/config"
	"github.com/lib/pq"
)

type Snowflake struct {
	conn       *sql.DB
	conf       *config.SnowflakeConfig
	syncSchema Schema
}

var (
	snowflakeSchemaMap = map[reflect.Type]string{
		reflect.TypeOf(int64(0)):    "BIGINT",
		reflect.TypeOf(""):          "VARCHAR(max)",
		reflect.TypeOf(time.Time{}): "TIMESTAMP",
	}
)

type columnConfig struct {
	DBName string
	DBType string
}

type snowflakeSchema []columnConfig

func (c snowflakeSchema) String() string {
	ss := make([]string, len(c))
	for i, f := range c {
		ss[i] = fmt.Sprintf("%s %s", f.DBName, f.DBType)
	}
	return strings.Join(ss, ",")
}

var _ Database = (*Snowflake)(nil)

func NewSnowflake(c *config.SnowflakeConfig) *Snowflake {
	return &Snowflake{
		conf:       c,
		syncSchema: MakeSchema(syncTable{}),
	}
}

func (rs *Snowflake) qualifiedExportTableName() string {
	if rs.conf.DatabaseSchema == "search_path" {
		return rs.conf.ExportTable
	}
	return fmt.Sprintf("%s.%s", rs.conf.DatabaseSchema, rs.conf.ExportTable)
}

func (rs *Snowflake) qualifiedSyncTableName() string {
	if rs.conf.DatabaseSchema == "search_path" {
		return rs.conf.SyncTable
	}
	return fmt.Sprintf("%s.%s", rs.conf.DatabaseSchema, rs.conf.SyncTable)
}

func (rs *Snowflake) getSchemaParameter() string {
	// the built-in current_schema() function will walk the Postgres search_path to get a schema name
	// more info: https://www.postgresql.org/docs/9.4/functions-info.html
	if rs.conf.DatabaseSchema == "search_path" {
		return "current_schema()"
	}

	return fmt.Sprintf("'%s'", rs.conf.DatabaseSchema)
}

func (rs *Snowflake) validateSchemaConfig() error {
	if rs.conf.DatabaseSchema == "" {
		return errors.New("DatabaseSchema definition missing from Snowflake configuration. More information: https://github.com/fullstorydev/hauser/blob/master/Snowflake.md#database-schema-configuration")
	}
	return nil
}

// GetExportTableColumns returns all the columns of the export table.
// It opens a connection and calls getTableColumns
func (rs *Snowflake) GetExportTableColumns() []string {
	var err error
	rs.conn, err = rs.MakeSnowflakeConnection()
	if err != nil {
		log.Fatal(err)
	}
	defer rs.conn.Close()

	return rs.getTableColumns(rs.conf.ExportTable)
}

func (rs *Snowflake) ValueToString(val interface{}, isTime bool) string {
	s := fmt.Sprintf("%v", val)
	if isTime {
		t, _ := time.Parse(time.RFC3339Nano, s)
		return t.String()
	}

	s = strings.Replace(s, "\n", " ", -1)
	s = strings.Replace(s, "\r", " ", -1)
	s = strings.Replace(s, "\x00", "", -1)

	if len(s) >= rs.conf.VarCharMax {
		s = s[:rs.conf.VarCharMax-1]
	}
	return s
}

func (rs *Snowflake) MakeSnowflakeConnection() (*sql.DB, error) {
	if err := rs.validateSchemaConfig(); err != nil {
		log.Fatal(err)
	}
	url := fmt.Sprintf("user=%v password=%v host=%v port=%v dbname=%v",
		rs.conf.User,
		rs.conf.Password,
		rs.conf.Host,
		rs.conf.Port,
		rs.conf.DB)

	var err error
	var db *sql.DB
	if db, err = sql.Open("postgres", url); err != nil {
		return nil, fmt.Errorf("snowflake connect error : (%v)", err)
	}

	if err = db.Ping(); err != nil {
		return nil, fmt.Errorf("snowflake ping error : (%v)", err)
	}
	return db, nil
}

func getBucketAndKey(bucketConfig, objName string) (string, string) {
	bucketParts := strings.Split(bucketConfig, "/")
	bucketName := bucketParts[0]
	keyPath := strings.Trim(strings.Join(bucketParts[1:], "/"), "/")
	key := strings.Trim(fmt.Sprintf("%s/%s", keyPath, objName), "/")

	return bucketName, key
}

func (rs *Snowflake) LoadToWarehouse(s3obj string, _ time.Time) error {
	var err error
	rs.conn, err = rs.MakeSnowflakeConnection()
	if err != nil {
		return err
	}
	defer rs.conn.Close()

	if err = rs.CopyInData(s3obj); err != nil {
		return err
	}

	return nil
}

func getColumnsToAdd(s Schema, existing []string) ([]columnConfig, error) {
	if len(s) < len(existing) {
		return nil, fmt.Errorf("incompatible schema: have %v, got %v", existing, s)
	}
	missing := make([]columnConfig, len(s)-len(existing))
	for i := 0; i < len(missing); i++ {
		field := s[len(existing)+i]
		missing[i] = columnConfig{
			DBName: field.DBName,
			DBType: snowflakeSchemaMap[field.FieldType],
		}
	}
	return missing, nil
}

func schemaToSnowflakeSchema(s Schema) snowflakeSchema {
	columns := make([]columnConfig, len(s))
	for i, field := range s {
		dbType, ok := snowflakeSchemaMap[field.FieldType]
		if !ok {
			panic(fmt.Sprintf("field %s does not have a mapping to a database type for snowflake", field.DBName))
		}
		columns[i] = columnConfig{
			DBName: field.DBName,
			DBType: dbType,
		}
	}
	return columns
}

func (rs *Snowflake) InitExportTable(schema Schema) (bool, error) {
	var err error
	rs.conn, err = rs.MakeSnowflakeConnection()
	if err != nil {
		return false, err
	}
	defer rs.conn.Close()

	if !rs.DoesTableExist(rs.conf.ExportTable) {
		// if the export table does not exist we create one with all the columns we expect!
		log.Printf("Export table %s does not exist! Creating one!", rs.qualifiedExportTableName())
		if err = rs.createExportTable(schema); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (rs *Snowflake) ApplyExportSchema(newSchema Schema) error {
	var err error
	rs.conn, err = rs.MakeSnowflakeConnection()
	if err != nil {
		return err
	}
	defer rs.conn.Close()

	existingColumns := rs.getTableColumns(rs.conf.ExportTable)
	missingFields, err := getColumnsToAdd(newSchema, existingColumns)
	if err != nil {
		return err
	}
	if len(missingFields) > 0 {
		log.Printf("Found %d missing fields. Adding columns for these fields.", len(missingFields))
		for _, f := range missingFields {
			// RS only allows addition of one column at a time, hence the the alter statements in a loop yuck
			// and I'm pretty sure Snowflake is the same way
			alterStmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s;", rs.qualifiedExportTableName(), f.DBName, f.DBType)
			if _, err = rs.conn.Exec(alterStmt); err != nil {
				return err
			}
		}
	}
	return nil
}

// CopyInData copies data from the given s3File to the export table
func (rs *Snowflake) CopyInData(s3file string) error {
	copyStatement := fmt.Sprintf("COPY %s FROM '%s' CREDENTIALS '%s' DELIMITER ',' REGION '%s' FORMAT AS CSV IGNOREHEADER 1 ACCEPTINVCHARS;",
		rs.qualifiedExportTableName(), s3file, rs.conf.Credentials, rs.conf.S3Region)
	_, err := rs.conn.Exec(copyStatement)
	return err
}

// CreateExportTable creates an export table with the hauser export table schema
func (rs *Snowflake) createExportTable(schema Schema) error {
	log.Printf("Creating table %s", rs.qualifiedExportTableName())

	stmt := fmt.Sprintf("create table %s(%s);", rs.qualifiedExportTableName(), schemaToSnowflakeSchema(schema))
	_, err := rs.conn.Exec(stmt)
	return err
}

// CreateSyncTable creates a sync table with the hauser sync table schema
func (rs *Snowflake) CreateSyncTable() error {
	log.Printf("Creating table %s", rs.qualifiedSyncTableName())

	stmt := fmt.Sprintf("create table %s(%s);", rs.qualifiedSyncTableName(), schemaToSnowflakeSchema(rs.syncSchema))
	_, err := rs.conn.Exec(stmt)
	return err
}

func (rs *Snowflake) SaveSyncPoint(_ context.Context, endTime time.Time) error {
	var err error
	rs.conn, err = rs.MakeSnowflakeConnection()
	if err != nil {
		log.Printf("Couldn't connect to DB: %s", err)
		return err
	}
	defer rs.conn.Close()

	insert := fmt.Sprintf("insert into %s values (%d, '%s', '%s')",
		rs.qualifiedSyncTableName(), -1, time.Now().UTC().Format(time.RFC3339), endTime.UTC().Format(time.RFC3339))
	if _, err := rs.conn.Exec(insert); err != nil {
		return err
	}

	return nil
}

func (rs *Snowflake) DeleteExportRecordsAfter(end time.Time) error {
	stmt := fmt.Sprintf("DELETE FROM %s where EventStart > '%s';",
		rs.qualifiedExportTableName(), end.Format(time.RFC3339))
	_, err := rs.conn.Exec(stmt)
	if err != nil {
		log.Printf("failed to delete from %s: %s", rs.qualifiedExportTableName(), err)
		return err
	}

	return nil
}

func (rs *Snowflake) LastSyncPoint(_ context.Context) (time.Time, error) {
	t := time.Time{}
	var err error
	rs.conn, err = rs.MakeSnowflakeConnection()
	if err != nil {
		log.Printf("Couldn't connect to DB: %s", err)
		return t, err
	}
	defer rs.conn.Close()

	if rs.DoesTableExist(rs.conf.SyncTable) {
		var syncTime pq.NullTime
		q := fmt.Sprintf("SELECT max(BundleEndTime) FROM %s;", rs.qualifiedSyncTableName())
		if err := rs.conn.QueryRow(q).Scan(&syncTime); err != nil {
			log.Printf("Couldn't get max(BundleEndTime): %s", err)
			return t, err
		}
		if syncTime.Valid {
			t = syncTime.Time
		}

		if err := rs.RemoveOrphanedRecords(syncTime); err != nil {
			return t, err
		}

	} else {
		if err := rs.CreateSyncTable(); err != nil {
			log.Printf("Couldn't create sync table: %s", err)
			return t, err
		}
	}
	return t, nil
}

func (rs *Snowflake) RemoveOrphanedRecords(lastSync pq.NullTime) error {
	if !rs.DoesTableExist(rs.conf.ExportTable) {
		// This is okay, because the hauser process will ensure that the table exists.
		return nil
	}

	// Find the time of the latest export record...if it's after
	// the time in the sync table, then there must have been a failure
	// after some records have been loaded, but before the sync record
	// was written. Use this as the latest sync time, and don't load
	// any records before this point to prevent duplication
	var exportTime pq.NullTime
	q := fmt.Sprintf("SELECT max(EventStart) FROM %s;", rs.qualifiedExportTableName())
	if err := rs.conn.QueryRow(q).Scan(&exportTime); err != nil {
		log.Printf("Couldn't get max(EventStart): %s", err)
		return err
	}
	if exportTime.Valid && exportTime.Time.After(lastSync.Time) {
		log.Printf("Export record timestamp after sync time (%s vs %s); cleaning",
			exportTime.Time, lastSync.Time)
		rs.DeleteExportRecordsAfter(lastSync.Time)
	}

	return nil
}

// DoesTableExist checks if a table with a given name exists
func (rs *Snowflake) DoesTableExist(name string) bool {
	log.Printf("Checking if table %s exists", name)

	var exists int
	query := fmt.Sprintf("SELECT count(*) FROM information_schema.tables WHERE table_schema = %s AND table_name = $1;", rs.getSchemaParameter())
	err := rs.conn.QueryRow(query, name).Scan(&exists)
	if err != nil {
		// something is horribly wrong...just give up
		log.Fatal(err)
	}
	return (exists != 0)
}

func (rs *Snowflake) getTableColumns(name string) []string {
	log.Printf("Fetching columns for table %s", name)
	ctx := context.Background()
	query := fmt.Sprintf("SELECT column_name FROM information_schema.columns WHERE table_schema = %s AND table_name = $1 order by ordinal_position;", rs.getSchemaParameter())
	rows, err := rs.conn.QueryContext(ctx, query, name)
	if err != nil {
		log.Fatal(err)
	}
	var columns []string

	defer rows.Close()
	for rows.Next() {
		var column string
		if err = rows.Scan(&column); err != nil {
			log.Fatal(err)
		}
		columns = append(columns, column)
	}

	// get any error encountered during iteration
	if err = rows.Err(); err != nil {
		log.Fatal(err)
	}
	return columns
}
