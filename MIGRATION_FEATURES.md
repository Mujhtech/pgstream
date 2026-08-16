# MySQL to PostgreSQL Migration Tool

> Product target and historical design notes. This file is not a list of completed guarantees. See `README.md` for the implemented and verified behavior.

A comprehensive migration tool that transfers your entire MySQL database to PostgreSQL, including all database objects and relationships.

## 🚀 Migration Process

The migration follows a structured 3-step process to ensure completeness and avoid dependency issues:

### **Step 1: Table Structure Creation**

- Creates all tables in PostgreSQL with proper data type mapping
- Handles MySQL-specific types (ENUM, DECIMAL, etc.)
- Creates PostgreSQL ENUM types from MySQL enum definitions
- Detects and converts common UUID primary-key representations
- Ensures all table structures are in place before proceeding

### **Step 2: Data Migration**

- Migrates data before secondary indexes, foreign keys, and triggers
- Uses keyset pagination for primary-keyed tables
- Persists a typed resume cursor after each committed batch

### **Step 3: Schema Object Migration**

- Migrates all indexes (primary, unique, regular)
- Migrates all foreign key constraints with proper rules
- Reports triggers that require manual PostgreSQL conversion
- Reports stored functions that require manual PostgreSQL conversion
- Migrates all views
- Ensures all relationships and constraints are established

## 🔧 Key Features

### 1. Table Structure Migration

- **Table Creation**: Automatically creates tables in PostgreSQL with proper data type mapping
- **Column Mapping**: Handles MySQL to PostgreSQL data type conversions
- **Enum Type Handling**: Creates PostgreSQL ENUM types from MySQL enum definitions
- **Schema Management**: Creates and uses specified PostgreSQL schemas

### 2. Data Type Conversions

- **Basic Types**:

  - `VARCHAR(n)` → `VARCHAR(n)`
  - `TEXT` → `TEXT`
  - `LONGTEXT` → `TEXT`
  - `INT` → `INTEGER`
  - `BIGINT` → `BIGINT`
  - `TINYINT(1)` → `BOOLEAN`
  - `DECIMAL(p,s)` → `NUMERIC(p,s)`
  - `DATETIME` → `TIMESTAMP`
  - `TIMESTAMP` → `TIMESTAMP`
  - `DATE` → `DATE`
  - `TIME` → `TIME`
  - `JSON` → `JSONB`
  - `BLOB` → `BYTEA`

- **Complex Types**:
  - `ENUM('A','B','C')` → `CREATE TYPE enum_name AS ENUM ('A','B','C')`
  - `DECIMAL(30,16)` → `NUMERIC(30,16)`
  - `CHAR(36)` → `UUID` (with auto-generation setup)
  - `VARCHAR(36)` → `UUID` (with auto-generation setup)
  - `BINARY(16)` → `UUID` (with auto-generation setup)
  - `VARBINARY(16)` → `UUID` (with auto-generation setup)

### 3. Schema Management

- **Schema Creation**: Automatically creates PostgreSQL schemas if they don't exist
- **Schema Isolation**: All objects are created in the specified schema
- **Cross-Database References**: Detected and rejected until an explicit target-schema mapping exists

### 4. UUID Primary Key Handling

- **Automatic Detection**: Detects MySQL tables with UUID primary keys (CHAR(36), VARCHAR(36), etc.)
- **Type Conversion**: Converts MySQL UUID columns to PostgreSQL UUID type
- **Cross-Reference Support**: Handles foreign keys referencing UUID primary keys

### 5. Index Migration

- **Primary Keys**: Automatically created during table creation
- **Unique Indexes**: Converted to PostgreSQL unique constraints
- **Regular Indexes**: Migrated with proper naming and options
- **Composite Indexes**: Preserved with correct column order

### 6. Foreign Key Migration

- **Constraint Names**: Preserved from MySQL
- **Referenced Tables**: Properly mapped to PostgreSQL
- **ON DELETE/ON UPDATE**: Rules preserved and converted
- **Cross-Database FKs**: Fail with an actionable error instead of being rebound to the wrong target schema

### 7. Trigger Migration

Target capability. The current implementation discovers and reports triggers but does not install generated trigger code, because a partial procedural SQL translation is unsafe.

### 8. Function Migration

Target capability. The current implementation discovers and reports stored functions for manual conversion.

### 9. View Migration

Target capability. The current implementation discovers and reports views for manual conversion; it does not rewrite MySQL SQL with unsafe string substitutions.

### 10. Data Validation and Transformation

- **Enum Validation**: Ensures data matches enum constraints
- **Type Validation**: Validates data types before insertion
- **Date/Time Validation**: Stops with the table, column, and value when MySQL data cannot be represented safely in PostgreSQL
- **NULL Handling**: Properly handles NULL values
- **Lossless Text Handling**: Preserves leading and trailing whitespace

### 11. Migration State Management

- **Resume Capability**: Primary-keyed tables resume from a typed keyset cursor; keyless tables require a fresh restart
- **Progress Tracking**: Tracks migration progress per table
- **Error Recovery**: Handles errors gracefully and continues
- **Status Tracking**: Monitors migration status (pending, in_progress, done, error)

## 📊 Performance Features

- **Batch Processing**: Processes data in configurable batch sizes
- **Progress Reporting**: Real-time progress updates
- **Memory Efficient**: Processes large datasets without memory issues
- **Concurrent Processing**: Can handle multiple tables (future enhancement)

## 🔍 Error Handling

- **Graceful Degradation**: Continues migration even when some objects fail
- **Detailed Logging**: Comprehensive error messages and debugging info
- **Data Validation**: Prevents invalid data from causing failures
- **Recovery Mechanisms**: Can resume interrupted migrations

## 📝 Usage Examples

### Basic Migration

```bash
./pgstream session
```

### With Custom Configuration

```bash
./pgstream session --batch-size 5000
```

## 🔧 Configuration

The tool supports configuration through:

- Command line arguments
- Environment variables
- Configuration files
- Interactive prompts

## 📋 Migration Logs

The tool provides detailed logging:

- 🔧 Schema migration progress
- 📦 Data migration progress
- ⚠️ Warning messages for non-critical issues
- ❌ Error messages with details
- ✅ Success confirmations
- 🔄 Resume operations

The implemented CLI favors explicit failures over lossy conversion. Objects that cannot yet be translated safely are reported for manual work rather than presented as successfully migrated.

## 🏗️ Migration Architecture

### **Phase 1: Structure Setup**

1. Create PostgreSQL schema
2. Get all MySQL tables
3. Create all table structures in PostgreSQL
4. Create all ENUM types

### **Phase 2: Data Transfer**

1. Migrate data for each table
2. Validate and transform data
3. Track progress and handle errors
4. Mark completion status

### **Phase 3: Schema Objects**

1. Migrate indexes for all tables
2. Migrate foreign keys for all tables
3. Discover and report triggers for manual conversion
4. Discover and report global functions for manual conversion
5. Discover and report global views for manual conversion

This structured approach ensures that all dependencies are in place before data migration begins, preventing incomplete migrations and dependency issues.

## 🔧 UUID Primary Key Example

### **MySQL Table Structure**

```sql
CREATE TABLE users (
  id CHAR(36) NOT NULL PRIMARY KEY,
  name VARCHAR(255) NOT NULL,
  email VARCHAR(255) UNIQUE,
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

### **PostgreSQL Migration Process**

1. **Detection**: Tool detects `id` column with `CHAR(36)` as UUID primary key
2. **Table Creation**: Creates the target column as native PostgreSQL `UUID`
3. **Data Validation**: Rejects any source value that is not a valid UUID instead of coercing it

### **Result**

- UUID primary keys are properly converted
- Foreign key references are preserved
- Data integrity is maintained
