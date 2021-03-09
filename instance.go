package tengo

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/VividCortex/mysqlerr"
	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/nozzle/throttler"
	"golang.org/x/sync/errgroup"
)

// Instance represents a single database server running on a specific host or address.
type Instance struct {
	BaseDSN        string // DSN ending in trailing slash; i.e. no schema name or params
	Driver         string
	User           string
	Password       string
	Host           string
	Port           int
	SocketPath     string
	defaultParams  map[string]string
	connectionPool map[string]*sqlx.DB // key is in format "schema?params"
	m              *sync.Mutex         // protects unexported fields for concurrent operations
	flavor         Flavor
	version        [3]int
	grants         []string
	waitTimeout    int
	maxUserConns   int
	bufferPoolSize int64
	sqlMode        []string
	valid          bool // true if any conn has ever successfully been made yet
}

// NewInstance returns a pointer to a new Instance corresponding to the
// supplied driver and dsn. Currently only "mysql" driver is supported.
// dsn should be formatted according to driver specifications. If it contains
// a schema name, it will be ignored. If it contains any params, they will be
// applied as default params to all connections (in addition to whatever is
// supplied in Connect).
func NewInstance(driver, dsn string) (*Instance, error) {
	if driver != "mysql" {
		return nil, fmt.Errorf("Unsupported driver \"%s\"", driver)
	}

	base := baseDSN(dsn)
	params := paramMap(dsn)
	parsedConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}

	instance := &Instance{
		BaseDSN:        base,
		Driver:         driver,
		User:           parsedConfig.User,
		Password:       parsedConfig.Passwd,
		defaultParams:  params,
		connectionPool: make(map[string]*sqlx.DB),
		flavor:         FlavorUnknown,
		m:              new(sync.Mutex),
	}

	switch parsedConfig.Net {
	case "unix":
		instance.Host = "localhost"
		instance.SocketPath = parsedConfig.Addr
	default:
		instance.Host, instance.Port, err = SplitHostOptionalPort(parsedConfig.Addr)
		if err != nil {
			return nil, err
		}
	}

	return instance, nil
}

// String for an instance returns a "host:port" string (or "localhost:/path/to/socket"
// if using UNIX domain socket)
func (instance *Instance) String() string {
	if instance.SocketPath != "" {
		return fmt.Sprintf("%s:%s", instance.Host, instance.SocketPath)
	} else if instance.Port == 0 {
		return instance.Host
	} else {
		return fmt.Sprintf("%s:%d", instance.Host, instance.Port)
	}
}

// HostAndOptionalPort is like String(), but omits the port if default
func (instance *Instance) HostAndOptionalPort() string {
	if instance.Port == 3306 || instance.SocketPath != "" {
		return instance.Host
	}
	return instance.String()
}

func (instance *Instance) buildParamString(params string) string {
	v := url.Values{}
	for defName, defValue := range instance.defaultParams {
		v.Set(defName, defValue)
	}
	overrides, _ := url.ParseQuery(params)
	for name := range overrides {
		v.Set(name, overrides.Get(name))
	}
	return v.Encode()
}

// ConnectionPool returns a new sqlx.DB for this instance's host/port/user/pass
// with the supplied default schema and params string. A connection attempt is
// made, and an error will be returned if connection fails.
// defaultSchema may be "" if it is not relevant.
// params should be supplied in format "foo=bar&fizz=buzz" with URL escaping
// already applied. Do not include a prefix of "?". params will be merged with
// instance.defaultParams, with params supplied here taking precedence.
// To avoid problems with unexpected disconnection, the connection pool will
// automatically have a max conn lifetime of at most 30sec, or less if a lower
// session wait_timeout was set in params, instance.defaultParams, or the DB's
// global wait_timeout variable.
func (instance *Instance) ConnectionPool(defaultSchema, params string) (*sqlx.DB, error) {
	fullParams := instance.buildParamString(params)
	return instance.rawConnectionPool(defaultSchema, fullParams, false)
}

// CachedConnectionPool operates like ConnectionPool, except it caches
// connection pools for reuse. When multiple requests are made for the same
// combination of defaultSchema and params, a pre-existing connection pool will
// be returned. See ConnectionPool for usage of the args for this method.
func (instance *Instance) CachedConnectionPool(defaultSchema, params string) (*sqlx.DB, error) {
	fullParams := instance.buildParamString(params)
	key := fmt.Sprintf("%s?%s", defaultSchema, fullParams)

	instance.m.Lock()
	defer instance.m.Unlock()
	if pool, ok := instance.connectionPool[key]; ok {
		return pool, nil
	}
	db, err := instance.rawConnectionPool(defaultSchema, fullParams, true)
	if err == nil {
		instance.connectionPool[key] = db
	}
	return db, err
}

// Connect is an alias for CachedConnectionPool.
func (instance *Instance) Connect(defaultSchema string, params string) (*sqlx.DB, error) {
	return instance.CachedConnectionPool(defaultSchema, params)
}

func (instance *Instance) rawConnectionPool(defaultSchema, fullParams string, alreadyLocked bool) (*sqlx.DB, error) {
	fullDSN := fmt.Sprintf("%s%s?%s", instance.BaseDSN, defaultSchema, fullParams)
	db, err := sqlx.Connect(instance.Driver, fullDSN)
	if err != nil {
		return nil, err
	}
	if !instance.valid {
		instance.hydrateVars(db, !alreadyLocked)
	}

	// Set max concurrent connections, ensuring it is less than any limit set on
	// the database side either globally or for this user. This does not completely
	// eliminate max-conn problems, because each Instance can have many separate
	// connection pools, but it may help.
	if instance.maxUserConns > 0 {
		if instance.maxUserConns < 12 {
			db.SetMaxOpenConns(2)
		} else {
			db.SetMaxOpenConns(instance.maxUserConns - 10)
		}
	}

	// Determine max conn lifetime, ensuring it is less than wait_timeout, and no
	// more than 30s.
	maxLifetime := 30 * time.Second
	if instance.waitTimeout > 1 && instance.waitTimeout <= 30 {
		maxLifetime = time.Duration(instance.waitTimeout-1) * time.Second
	} else if instance.waitTimeout == 1 {
		maxLifetime = 900 * time.Millisecond
	}
	db.SetConnMaxLifetime(maxLifetime)
	return db.Unsafe(), nil
}

// CanConnect returns true if the Instance can currently be connected to, using
// its configured User and Password. If a new connection cannot be made, the
// return value will be false, along with an error expressing the reason.
func (instance *Instance) CanConnect() (bool, error) {
	db, err := instance.ConnectionPool("", "")
	if db != nil {
		db.Close() // close immediately to avoid a buildup of sleeping idle conns
	}
	return err == nil, err
}

// Valid returns true if a successful connection can be made to the Instance,
// or if a successful connection has already been made previously. This method
// only returns false if no previous successful connection was ever made, and a
// new attempt to establish one fails.
func (instance *Instance) Valid() (bool, error) {
	if instance.valid {
		return true, nil
	}
	// CachedConnectionPool establishes one conn in the pool; if
	// successful, this also calls hydrateVars which then sets valid to true
	_, err := instance.CachedConnectionPool("", "")
	return err == nil, err
}

// CloseAll closes all of instance's cached connection pools. This can be
// useful for graceful shutdown, to avoid aborted-connection counters/logging
// in some versions of MySQL.
func (instance *Instance) CloseAll() {
	instance.m.Lock()
	for key, db := range instance.connectionPool {
		db.Close()
		delete(instance.connectionPool, key)
	}
	instance.m.Unlock()
}

// Flavor returns this instance's flavor value, representing the database
// distribution/fork/vendor as well as major and minor version. If this is
// unable to be determined or an error occurs, FlavorUnknown will be returned.
func (instance *Instance) Flavor() Flavor {
	instance.Valid() // force an attempt to hydrate flavor, if not done already
	return instance.flavor
}

// SetFlavor attempts to set this instance's flavor value. If the instance's
// flavor has already been hydrated successfully, the value is not changed and
// an error is returned.
func (instance *Instance) SetFlavor(flavor Flavor) error {
	if instance.flavor.Known() {
		return fmt.Errorf("SetFlavor: instance %s already detected as flavor %s", instance, instance.flavor)
	}
	instance.ForceFlavor(flavor)
	return nil
}

// ForceFlavor overrides this instance's flavor value. Only tests should call
// this method directly; all other callers should use SetFlavor instead and
// check the error return value.
func (instance *Instance) ForceFlavor(flavor Flavor) {
	instance.flavor = flavor
	instance.version = [3]int{flavor.Major, flavor.Minor, flavor.Patch}
}

// Version returns three ints representing the database's major, minor, and
// patch version, respectively. If this is unable to be determined, all 0's
// will be returned.
func (instance *Instance) Version() (int, int, int) {
	instance.Valid() // force an attempt to hydrate version, if not done already
	return instance.version[0], instance.version[1], instance.version[2]
}

// hydrateVars populates several non-exported Instance fields by querying
// various global and session variables. Failures are ignored; these variables
// are designed to help inform behavior but are not strictly mandatory.
func (instance *Instance) hydrateVars(db *sqlx.DB, lock bool) {
	var err error
	if lock {
		instance.m.Lock()
		defer instance.m.Unlock()
		if instance.valid {
			return
		}
	}
	var result struct {
		VersionComment string
		Version        string
		SQLMode        string
		WaitTimeout    int
		MaxUserConns   int
		MaxConns       int
		BufferPoolSize int64
	}
	query := `
		SELECT @@global.version_comment AS versioncomment,
		       @@global.version AS version,
		       @@session.sql_mode AS sqlmode,
		       @@session.wait_timeout AS waittimeout,
		       @@global.innodb_buffer_pool_size AS bufferpoolsize,
		       @@session.max_user_connections AS maxuserconns,
		       @@global.max_connections AS maxconns`
	if err = db.Get(&result, query); err != nil {
		return
	}
	instance.valid = true
	instance.version = ParseVersion(result.Version)
	instance.flavor = ParseFlavor(result.Version, result.VersionComment)
	instance.sqlMode = strings.Split(result.SQLMode, ",")
	instance.waitTimeout = result.WaitTimeout
	instance.bufferPoolSize = result.BufferPoolSize
	if result.MaxUserConns > 0 {
		instance.maxUserConns = result.MaxUserConns
	} else {
		instance.maxUserConns = result.MaxConns
	}
}

// Regular expression defining privileges that allow use of setting session
// variable sql_log_bin. Note that SESSION_VARIABLES_ADMIN and
// SYSTEM_VARIABLES_ADMIN are from MySQL 8.0+. Meanwhile BINLOG ADMIN is from
// MariaDB 10.5+ as per https://jira.mariadb.org/browse/MDEV-21957; note the
// space in the name (not to be confused with BINLOG_ADMIN with an underscore,
// which is a MySQL 8.0 privilege which does NOT control sql_log_bin!)
var reSkipBinlog = regexp.MustCompile(`(?:ALL PRIVILEGES ON \*\.\*|SUPER|SESSION_VARIABLES_ADMIN|SYSTEM_VARIABLES_ADMIN|BINLOG ADMIN)[,\s]`)

// CanSkipBinlog returns true if instance.User has privileges necessary to
// set sql_log_bin=0. If an error occurs in checking grants, this method returns
// false as a safe fallback.
func (instance *Instance) CanSkipBinlog() bool {
	if instance.grants == nil {
		instance.hydrateGrants()
	}
	for _, grant := range instance.grants {
		if reSkipBinlog.MatchString(grant) {
			return true
		}
	}
	return false
}

func (instance *Instance) hydrateGrants() {
	db, err := instance.CachedConnectionPool("", "")
	if err != nil {
		return
	}
	instance.m.Lock()
	defer instance.m.Unlock()
	db.Select(&instance.grants, "SHOW GRANTS")
}

// SchemaNames returns a slice of all schema name strings on the instance
// visible to the user. System schemas are excluded.
func (instance *Instance) SchemaNames() ([]string, error) {
	db, err := instance.CachedConnectionPool("", "")
	if err != nil {
		return nil, err
	}
	var result []string
	query := `
		SELECT schema_name
		FROM   information_schema.schemata
		WHERE  schema_name NOT IN ('information_schema', 'performance_schema', 'mysql', 'test', 'sys')`
	if err := db.Select(&result, query); err != nil {
		return nil, err
	}
	return result, nil
}

// Schemas returns a slice of schemas on the instance visible to the user. If
// called with no args, all non-system schemas will be returned. Or pass one or
// more schema names as args to filter the result to just those schemas.
// Note that the ordering of the resulting slice is not guaranteed.
func (instance *Instance) Schemas(onlyNames ...string) ([]*Schema, error) {
	db, err := instance.CachedConnectionPool("", "")
	if err != nil {
		return nil, err
	}
	var rawSchemas []struct {
		Name      string `db:"schema_name"`
		CharSet   string `db:"default_character_set_name"`
		Collation string `db:"default_collation_name"`
	}

	var args []interface{}
	var query string

	// Note on these queries: MySQL 8.0 changes information_schema column names to
	// come back from queries in all caps, so we need to explicitly use AS clauses
	// in order to get them back as lowercase and have sqlx Select() work
	if len(onlyNames) == 0 {
		query = `
			SELECT schema_name AS schema_name, default_character_set_name AS default_character_set_name,
			       default_collation_name AS default_collation_name
			FROM   information_schema.schemata
			WHERE  schema_name NOT IN ('information_schema', 'performance_schema', 'mysql', 'test', 'sys')`
	} else {
		query = `
			SELECT schema_name AS schema_name, default_character_set_name AS default_character_set_name,
			       default_collation_name AS default_collation_name
			FROM   information_schema.schemata
			WHERE  schema_name IN (?)`
		query, args, err = sqlx.In(query, onlyNames)
	}
	if err := db.Select(&rawSchemas, query, args...); err != nil {
		return nil, err
	}

	schemas := make([]*Schema, len(rawSchemas))
	for n, rawSchema := range rawSchemas {
		schemas[n] = &Schema{
			Name:      rawSchema.Name,
			CharSet:   rawSchema.CharSet,
			Collation: rawSchema.Collation,
		}
		// Create a non-cached connection pool with this schema as the default
		// database. The instance.querySchemaX calls below can establish a lot of
		// connections, so we will explicitly close the pool afterwards, to avoid
		// keeping a very large number of conns open. (Although idle conns eventually
		// get closed automatically, this may take too long.)
		schemaDB, err := instance.ConnectionPool(rawSchema.Name, instance.introspectionParams())
		if err != nil {
			return nil, err
		}
		flavor := instance.Flavor()
		if instance.maxUserConns >= 30 {
			// Limit concurrency to 20, unless limit is already lower than this due to
			// having a low maxUserConns (see logic in Instance.rawConnectionPool)
			schemaDB.SetMaxOpenConns(20)
		}
		g, ctx := errgroup.WithContext(context.Background())
		g.Go(func() (err error) {
			schemas[n].Tables, err = querySchemaTables(ctx, schemaDB, rawSchema.Name, flavor)
			return err
		})
		g.Go(func() (err error) {
			schemas[n].Routines, err = querySchemaRoutines(ctx, schemaDB, rawSchema.Name, flavor)
			return err
		})
		err = g.Wait()
		schemaDB.Close()
		if err != nil {
			return nil, err
		}
	}
	return schemas, nil
}

// SchemasByName returns a map of schema name string to *Schema.  If
// called with no args, all non-system schemas will be returned. Or pass one or
// more schema names as args to filter the result to just those schemas.
func (instance *Instance) SchemasByName(onlyNames ...string) (map[string]*Schema, error) {
	schemas, err := instance.Schemas(onlyNames...)
	if err != nil {
		return nil, err
	}
	result := make(map[string]*Schema, len(schemas))
	for _, s := range schemas {
		result[s.Name] = s
	}
	return result, nil
}

// Schema returns a single schema by name. If the schema does not exist, nil
// will be returned along with a sql.ErrNoRows error.
func (instance *Instance) Schema(name string) (*Schema, error) {
	schemas, err := instance.Schemas(name)
	if err != nil {
		return nil, err
	} else if len(schemas) == 0 {
		return nil, sql.ErrNoRows
	}
	return schemas[0], nil
}

// HasSchema returns true if this instance has a schema with the supplied name
// visible to the user, or false otherwise. An error result will only be
// returned if a connection or query failed entirely and we weren't able to
// determine whether the schema exists.
func (instance *Instance) HasSchema(name string) (bool, error) {
	db, err := instance.CachedConnectionPool("", "")
	if err != nil {
		return false, err
	}
	var exists int
	query := `
		SELECT 1
		FROM   information_schema.schemata
		WHERE  schema_name = ?`
	err = db.Get(&exists, query, name)
	if err == nil {
		return true, nil
	} else if err == sql.ErrNoRows {
		return false, nil
	} else {
		return false, err
	}
}

// ShowCreateTable returns a string with a CREATE TABLE statement, representing
// how the instance views the specified table as having been created.
func (instance *Instance) ShowCreateTable(schema, table string) (string, error) {
	db, err := instance.CachedConnectionPool(schema, instance.introspectionParams())
	if err != nil {
		return "", err
	}
	return showCreateTable(context.Background(), db, table)
}

// introspectionParams returns a params string which ensures safe session
// variables for use with SHOW CREATE as well as queries on information_schema
func (instance *Instance) introspectionParams() string {
	v := url.Values{}
	v.Set("sql_quote_show_create", "1")

	// In MySQL 8, ensure we get up-to-date values for table sizes as well as next
	// auto_increment value
	if instance.Flavor().HasDataDictionary() {
		v.Set("information_schema_stats_expiry", "0")
	}

	keepMode := make([]string, 0, len(instance.sqlMode))
	for _, mode := range instance.sqlMode {
		// Strip out these problematic modes: ANSI, ANSI_QUOTES, NO_FIELD_OPTIONS, NO_KEY_OPTIONS, NO_TABLE_OPTIONS
		if strings.HasPrefix(mode, "ANSI") || (strings.HasPrefix(mode, "NO_") && strings.HasSuffix(mode, "_OPTIONS")) {
			continue
		}
		keepMode = append(keepMode, mode)
	}
	if len(keepMode) != len(instance.sqlMode) {
		v.Set("sql_mode", fmt.Sprintf("'%s'", strings.Join(keepMode, ",")))
	}

	return v.Encode()
}

func showCreateTable(ctx context.Context, db *sqlx.DB, table string) (string, error) {
	var row struct {
		TableName       string `db:"Table"`
		CreateStatement string `db:"Create Table"`
	}
	query := fmt.Sprintf("SHOW CREATE TABLE %s", EscapeIdentifier(table))
	if err := db.GetContext(ctx, &row, query); err != nil {
		return "", err
	}
	return row.CreateStatement, nil
}

// TableSize returns an estimate of the table's size on-disk, based on data in
// information_schema. If the table or schema does not exist on this instance,
// the error will be sql.ErrNoRows.
// Please note that use of innodb_stats_persistent may negatively impact the
// accuracy. For example, see https://bugs.mysql.com/bug.php?id=75428.
func (instance *Instance) TableSize(schema, table string) (int64, error) {
	var result int64
	db, err := instance.CachedConnectionPool("", instance.introspectionParams())
	if err != nil {
		return 0, err
	}
	err = db.Get(&result, `
		SELECT  data_length + index_length + data_free
		FROM    information_schema.tables
		WHERE   table_schema = ? and table_name = ?`,
		schema, table)
	return result, err
}

// TableHasRows returns true if the table has at least one row. If an error
// occurs in querying, also returns true (along with the error) since a false
// positive is generally less dangerous in this case than a false negative.
func (instance *Instance) TableHasRows(schema, table string) (bool, error) {
	db, err := instance.CachedConnectionPool(schema, "")
	if err != nil {
		return true, err
	}
	return tableHasRows(db, table)
}

func tableHasRows(db *sqlx.DB, table string) (bool, error) {
	var result []int
	query := fmt.Sprintf("SELECT 1 FROM %s LIMIT 1", EscapeIdentifier(table))
	if err := db.Select(&result, query); err != nil {
		return true, err
	}
	return len(result) != 0, nil
}

func confirmTablesEmpty(db *sqlx.DB, tables []string) error {
	th := throttler.New(15, len(tables))
	for _, name := range tables {
		go func(name string) {
			hasRows, err := tableHasRows(db, name)
			if err == nil && hasRows {
				err = fmt.Errorf("table %s has at least one row", EscapeIdentifier(name))
			}
			th.Done(err)
		}(name)
		if th.Throttle() > 0 {
			return th.Errs()[0]
		}
	}
	return nil
}

// SchemaCreationOptions specifies schema-level metadata when creating or
// altering a database.
type SchemaCreationOptions struct {
	DefaultCharSet   string
	DefaultCollation string
	SkipBinlog       bool
}

func (opts SchemaCreationOptions) params() string {
	if opts.SkipBinlog {
		return "sql_log_bin=0"
	}
	return ""
}

// CreateSchema creates a new database schema with the supplied name, and
// optionally the supplied default CharSet and Collation. (Leave these fields
// blank to use server defaults.)
func (instance *Instance) CreateSchema(name string, opts SchemaCreationOptions) (*Schema, error) {
	db, err := instance.CachedConnectionPool("", opts.params())
	if err != nil {
		return nil, err
	}
	// Technically the server defaults would be used anyway if these are left
	// blank, but we need the returned Schema value to reflect the correct values,
	// and we can avoid re-querying this way
	if opts.DefaultCharSet == "" || opts.DefaultCollation == "" {
		defCharSet, defCollation, err := instance.DefaultCharSetAndCollation()
		if err != nil {
			return nil, err
		}
		if opts.DefaultCharSet == "" {
			opts.DefaultCharSet = defCharSet
		}
		if opts.DefaultCollation == "" {
			opts.DefaultCollation = defCollation
		}
	}
	schema := &Schema{
		Name:      name,
		CharSet:   opts.DefaultCharSet,
		Collation: opts.DefaultCollation,
		Tables:    []*Table{},
	}
	_, err = db.Exec(schema.CreateStatement())
	if err != nil {
		return nil, err
	}
	return schema, nil
}

// DropSchema first drops all tables in the schema, and then drops the database
// schema itself. If opts.OnlyIfEmpty==true, returns an error if any of the
// tables have any rows.
func (instance *Instance) DropSchema(schema string, opts BulkDropOptions) error {
	err := instance.DropTablesInSchema(schema, opts)
	if err != nil {
		return err
	}

	// No need to actually obtain the fully hydrated schema value; we already know
	// it has no tables after the call above, and the schema's name alone is
	// sufficient to call Schema.DropStatement() to generate the necessary SQL
	s := &Schema{
		Name: schema,
	}
	db, err := instance.CachedConnectionPool("", opts.params())
	if err != nil {
		return err
	}
	_, err = db.Exec(s.DropStatement())
	if err != nil {
		return err
	}

	prefix := fmt.Sprintf("%s?", schema)
	instance.m.Lock()
	defer instance.m.Unlock()
	for key, connPool := range instance.connectionPool {
		if strings.HasPrefix(key, prefix) {
			connPool.Close()
			delete(instance.connectionPool, key)
		}
	}
	return nil
}

// AlterSchema changes the character set and/or collation of the supplied schema
// on instance. Supply an empty string for opts.DefaultCharSet to only change
// the collation, or supply an empty string for opts.DefaultCollation to use the
// default collation of opts.DefaultCharSet. (Supplying an empty string for both
// is also allowed, but is a no-op.)
func (instance *Instance) AlterSchema(schema string, opts SchemaCreationOptions) error {
	s, err := instance.Schema(schema)
	if err != nil {
		return err
	}
	statement := s.AlterStatement(opts.DefaultCharSet, opts.DefaultCollation)
	if statement == "" {
		return nil
	}
	db, err := instance.CachedConnectionPool("", opts.params())
	if err != nil {
		return err
	}
	if _, err = db.Exec(statement); err != nil {
		return err
	}
	return nil
}

// BulkDropOptions controls how objects are dropped in bulk.
type BulkDropOptions struct {
	OnlyIfEmpty     bool // If true, when dropping tables, error if any have rows
	MaxConcurrency  int  // Max objects to drop at once
	SkipBinlog      bool // If true, use session sql_log_bin=0 (requires superuser)
	PartitionsFirst bool // If true, drop RANGE/LIST partitioned tables one partition at a time
}

func (opts BulkDropOptions) params() string {
	if opts.SkipBinlog {
		return "foreign_key_checks=0&sql_log_bin=0"
	}
	return "foreign_key_checks=0"
}

// Concurrency returns the concurrency, with a minimum value of 1.
func (opts BulkDropOptions) Concurrency() int {
	if opts.MaxConcurrency < 1 {
		return 1
	}
	return opts.MaxConcurrency
}

// DropTablesInSchema drops all tables in a schema. If opts.OnlyIfEmpty==true,
// returns an error if any of the tables have any rows.
func (instance *Instance) DropTablesInSchema(schema string, opts BulkDropOptions) error {
	db, err := instance.CachedConnectionPool(schema, opts.params())
	if err != nil {
		return err
	}

	// Obtain table and partition names
	tableMap, err := tablesToPartitions(db, schema)
	if err != nil {
		return err
	} else if len(tableMap) == 0 {
		return nil
	}

	// If requested, confirm tables are empty
	if opts.OnlyIfEmpty {
		names := make([]string, 0, len(tableMap))
		for tableName := range tableMap {
			names = append(names, tableName)
		}
		if err := confirmTablesEmpty(db, names); err != nil {
			return err
		}
	}

	// If buffer pool is over 32GB and flavor doesn't have optimized DROP TABLE,
	// reduce drop concurrency to 1 to reduce risk of stalls
	concurrency := opts.Concurrency()
	if instance.bufferPoolSize >= (32*1024*1024*1024) && !instance.flavor.MySQLishMinVersion(8, 0, 23) {
		concurrency = 1
	}
	th := throttler.New(concurrency, len(tableMap))
	retries := make(chan string, len(tableMap))
	for name, partitions := range tableMap {
		go func(name string, partitions []string) {
			var err error
			if len(partitions) > 1 && opts.PartitionsFirst {
				err = dropPartitions(db, name, partitions[0:len(partitions)-1])
			}
			if err == nil {
				_, err := db.Exec(fmt.Sprintf("DROP TABLE %s", EscapeIdentifier(name)))
				// With the new data dictionary added in MySQL 8.0, attempting to
				// concurrently drop two tables that have a foreign key constraint between
				// them can deadlock.
				if IsDatabaseError(err, mysqlerr.ER_LOCK_DEADLOCK) {
					retries <- name
					err = nil
				}
			}
			th.Done(err)
		}(name, partitions)
		th.Throttle()
	}
	close(retries)
	for name := range retries {
		if _, err := db.Exec(fmt.Sprintf("DROP TABLE %s", EscapeIdentifier(name))); err != nil {
			return err
		}
	}
	if errs := th.Errs(); len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// DropRoutinesInSchema drops all stored procedures and functions in a schema.
func (instance *Instance) DropRoutinesInSchema(schema string, opts BulkDropOptions) error {
	db, err := instance.CachedConnectionPool(schema, opts.params())
	if err != nil {
		return err
	}

	// Obtain names and types directly; faster than going through
	// instance.Schema(schema) since we don't need other introspection
	var routineInfo []struct {
		Name string `db:"routine_name"`
		Type string `db:"routine_type"`
	}
	query := `
		SELECT routine_name AS routine_name, UPPER(routine_type) AS routine_type
		FROM   information_schema.routines
		WHERE  routine_schema = ?`
	if err := db.Select(&routineInfo, query, schema); err != nil {
		return err
	} else if len(routineInfo) == 0 {
		return nil
	}

	th := throttler.New(opts.Concurrency(), len(routineInfo))
	for _, ri := range routineInfo {
		go func(name, typ string) {
			_, err := db.Exec(fmt.Sprintf("DROP %s %s", typ, EscapeIdentifier(name)))
			th.Done(err)
		}(ri.Name, ri.Type)
		th.Throttle()
	}
	if errs := th.Errs(); len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// tablesToPartitions returns a map whose keys are all tables in the schema
// (whether partitioned or not), and values are either nil (if unpartitioned or
// partitioned in a way that doesn't support DROP PARTITION) or a slice of
// partition names (if using RANGE or LIST partitioning). Views are excluded
// from the result.
func tablesToPartitions(db *sqlx.DB, schema string) (map[string][]string, error) {
	// information_schema.partitions contains all tables (not just partitioned)
	// and excludes views (which we don't want here anyway)
	var rawNames []struct {
		TableName     string         `db:"table_name"`
		PartitionName sql.NullString `db:"partition_name"`
		Method        sql.NullString `db:"partition_method"`
		SubMethod     sql.NullString `db:"subpartition_method"`
		Position      sql.NullInt64  `db:"partition_ordinal_position"`
	}
	// Explicit AS clauses needed for compatibility with MySQL 8 data dictionary,
	// otherwise results come back with uppercase col names, breaking Select
	query := `
		SELECT   SQL_BUFFER_RESULT
		         p.table_name AS table_name, p.partition_name AS partition_name,
		         p.partition_method AS partition_method,
		         p.subpartition_method AS subpartition_method,
		         p.partition_ordinal_position AS partition_ordinal_position
		FROM     information_schema.partitions p
		WHERE    p.table_schema = ?
		ORDER BY p.table_name, p.partition_ordinal_position`
	if err := db.Select(&rawNames, query, schema); err != nil {
		return nil, err
	}

	partitions := make(map[string][]string)
	for _, rn := range rawNames {
		if !rn.Position.Valid || rn.Position.Int64 == 1 {
			partitions[rn.TableName] = nil
		}
		if rn.Method.Valid && !rn.SubMethod.Valid &&
			(strings.HasPrefix(rn.Method.String, "RANGE") || strings.HasPrefix(rn.Method.String, "LIST")) {
			partitions[rn.TableName] = append(partitions[rn.TableName], rn.PartitionName.String)
		}
	}
	return partitions, nil
}

func dropPartitions(db *sqlx.DB, table string, partitions []string) error {
	for _, partName := range partitions {
		_, err := db.Exec(fmt.Sprintf("ALTER TABLE %s DROP PARTITION %s",
			EscapeIdentifier(table),
			EscapeIdentifier(partName)))
		if err != nil {
			return err
		}
	}
	return nil
}

// DefaultCharSetAndCollation returns the instance's default character set and
// collation
func (instance *Instance) DefaultCharSetAndCollation() (serverCharSet, serverCollation string, err error) {
	db, err := instance.CachedConnectionPool("", "")
	if err != nil {
		return
	}
	err = db.QueryRow("SELECT @@global.character_set_server, @@global.collation_server").Scan(&serverCharSet, &serverCollation)
	return
}
