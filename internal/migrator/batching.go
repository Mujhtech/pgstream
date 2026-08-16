package migrator

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	maxPostgresParameters = 65535
	maxRowsPerInsert      = 1000
)

type cursorComponent struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

func quoteMySQLIdentifier(identifier string) string {
	return "`" + strings.ReplaceAll(identifier, "`", "``") + "`"
}

func quotePostgresIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func quotePostgresLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func buildMySQLStreamingQuery(table string, columns []string) (string, error) {
	if table == "" {
		return "", fmt.Errorf("table name cannot be empty")
	}
	if len(columns) == 0 {
		return "", fmt.Errorf("at least one column is required")
	}
	quotedColumns := make([]string, len(columns))
	for i, column := range columns {
		quotedColumns[i] = quoteMySQLIdentifier(column)
	}
	return fmt.Sprintf("SELECT %s FROM %s", strings.Join(quotedColumns, ", "), quoteMySQLIdentifier(table)), nil
}

func buildMySQLBatchQuery(table string, columns, keyColumns []string, cursor []any, offset int64, batchSize int) (string, []any, error) {
	if table == "" {
		return "", nil, fmt.Errorf("table name cannot be empty")
	}
	if len(columns) == 0 {
		return "", nil, fmt.Errorf("at least one column is required")
	}
	if batchSize <= 0 {
		return "", nil, fmt.Errorf("batch size must be greater than zero")
	}
	if len(keyColumns) == 0 {
		return "", nil, fmt.Errorf("paginated batch queries require a primary key; use one streaming query for keyless tables")
	}
	if len(cursor) > 0 && len(cursor) != len(keyColumns) {
		return "", nil, fmt.Errorf("cursor has %d values for %d key columns", len(cursor), len(keyColumns))
	}

	quotedColumns := make([]string, len(columns))
	for i, column := range columns {
		quotedColumns[i] = quoteMySQLIdentifier(column)
	}

	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(quotedColumns, ", "), quoteMySQLIdentifier(table))
	args := make([]any, 0, len(keyColumns)*2+2)

	if len(cursor) > 0 {
		predicate, predicateArgs := buildKeysetPredicate(keyColumns, cursor)
		query += " WHERE " + predicate
		args = append(args, predicateArgs...)
	}

	quotedKeys := make([]string, len(keyColumns))
	for i, key := range keyColumns {
		quotedKeys[i] = quoteMySQLIdentifier(key)
	}
	query += " ORDER BY " + strings.Join(quotedKeys, ", ")

	query += " LIMIT ?"
	args = append(args, batchSize)
	if len(cursor) == 0 && offset > 0 {
		query += " OFFSET ?"
		args = append(args, offset)
	}

	return query, args, nil
}

func buildKeysetPredicate(keyColumns []string, cursor []any) (string, []any) {
	branches := make([]string, 0, len(keyColumns))
	args := make([]any, 0, len(keyColumns)*(len(keyColumns)+1)/2)

	for i := range keyColumns {
		parts := make([]string, 0, i+1)
		for previous := 0; previous < i; previous++ {
			parts = append(parts, quoteMySQLIdentifier(keyColumns[previous])+" = ?")
			args = append(args, cursor[previous])
		}
		parts = append(parts, quoteMySQLIdentifier(keyColumns[i])+" > ?")
		args = append(args, cursor[i])
		branches = append(branches, "("+strings.Join(parts, " AND ")+")")
	}

	return "(" + strings.Join(branches, " OR ") + ")", args
}

func encodeCursor(values []any) (string, error) {
	components := make([]cursorComponent, len(values))
	for i, value := range values {
		component, err := encodeCursorComponent(value)
		if err != nil {
			return "", fmt.Errorf("encode cursor component %d: %w", i, err)
		}
		components[i] = component
	}

	encoded, err := json.Marshal(components)
	if err != nil {
		return "", fmt.Errorf("encode cursor: %w", err)
	}
	return string(encoded), nil
}

func encodeCursorComponent(value any) (cursorComponent, error) {
	switch value := value.(type) {
	case nil:
		return cursorComponent{}, fmt.Errorf("primary key cursor cannot contain NULL")
	case []byte:
		return cursorComponent{Type: "bytes", Value: base64.StdEncoding.EncodeToString(value)}, nil
	case string:
		return cursorComponent{Type: "string", Value: value}, nil
	case int:
		return cursorComponent{Type: "int64", Value: strconv.FormatInt(int64(value), 10)}, nil
	case int8:
		return cursorComponent{Type: "int64", Value: strconv.FormatInt(int64(value), 10)}, nil
	case int16:
		return cursorComponent{Type: "int64", Value: strconv.FormatInt(int64(value), 10)}, nil
	case int32:
		return cursorComponent{Type: "int64", Value: strconv.FormatInt(int64(value), 10)}, nil
	case int64:
		return cursorComponent{Type: "int64", Value: strconv.FormatInt(value, 10)}, nil
	case uint:
		return cursorComponent{Type: "uint64", Value: strconv.FormatUint(uint64(value), 10)}, nil
	case uint8:
		return cursorComponent{Type: "uint64", Value: strconv.FormatUint(uint64(value), 10)}, nil
	case uint16:
		return cursorComponent{Type: "uint64", Value: strconv.FormatUint(uint64(value), 10)}, nil
	case uint32:
		return cursorComponent{Type: "uint64", Value: strconv.FormatUint(uint64(value), 10)}, nil
	case uint64:
		return cursorComponent{Type: "uint64", Value: strconv.FormatUint(value, 10)}, nil
	case float32:
		return cursorComponent{Type: "float64", Value: strconv.FormatFloat(float64(value), 'g', -1, 64)}, nil
	case float64:
		return cursorComponent{Type: "float64", Value: strconv.FormatFloat(value, 'g', -1, 64)}, nil
	case bool:
		return cursorComponent{Type: "bool", Value: strconv.FormatBool(value)}, nil
	case time.Time:
		return cursorComponent{Type: "time", Value: value.Format(time.RFC3339Nano)}, nil
	default:
		return cursorComponent{}, fmt.Errorf("unsupported cursor value type %T", value)
	}
}

func decodeCursor(encoded string) ([]any, error) {
	if encoded == "" {
		return nil, nil
	}

	var components []cursorComponent
	if err := json.Unmarshal([]byte(encoded), &components); err != nil {
		return nil, fmt.Errorf("decode cursor: %w", err)
	}

	values := make([]any, len(components))
	for i, component := range components {
		value, err := decodeCursorComponent(component)
		if err != nil {
			return nil, fmt.Errorf("decode cursor component %d: %w", i, err)
		}
		values[i] = value
	}
	return values, nil
}

func decodeCursorComponent(component cursorComponent) (any, error) {
	switch component.Type {
	case "bytes":
		return base64.StdEncoding.DecodeString(component.Value)
	case "string":
		return component.Value, nil
	case "int64":
		return strconv.ParseInt(component.Value, 10, 64)
	case "uint64":
		return strconv.ParseUint(component.Value, 10, 64)
	case "float64":
		return strconv.ParseFloat(component.Value, 64)
	case "bool":
		return strconv.ParseBool(component.Value)
	case "time":
		return time.Parse(time.RFC3339Nano, component.Value)
	default:
		return nil, fmt.Errorf("unsupported cursor component type %q", component.Type)
	}
}

func extractCursor(row []any, columns, keyColumns []string) ([]any, error) {
	values := make([]any, len(keyColumns))
	for i, keyColumn := range keyColumns {
		found := false
		for columnIndex, column := range columns {
			if strings.EqualFold(column, keyColumn) {
				if columnIndex >= len(row) {
					return nil, fmt.Errorf("row is missing primary key column %q", keyColumn)
				}
				values[i] = row[columnIndex]
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("primary key column %q is not in the selected columns", keyColumn)
		}
	}
	return values, nil
}

func buildPostgresInsert(schema, table string, columns, conflictColumns []string, rows [][]any) (string, []any, error) {
	if len(columns) == 0 {
		return "", nil, fmt.Errorf("at least one insert column is required")
	}
	if len(rows) == 0 {
		return "", nil, fmt.Errorf("at least one insert row is required")
	}

	quotedColumns := make([]string, len(columns))
	for i, column := range columns {
		quotedColumns[i] = quotePostgresIdentifier(column)
	}

	// The VALUES clause is assembled into one builder with strconv-formatted
	// placeholders; per-placeholder fmt.Sprintf plus per-row joins dominated
	// this function's profile (see hotpath_bench_test.go).
	var query strings.Builder
	query.Grow(64 + len(rows)*(len(columns)*6+4))
	query.WriteString("INSERT INTO ")
	query.WriteString(quotePostgresIdentifier(schema))
	query.WriteByte('.')
	query.WriteString(quotePostgresIdentifier(table))
	query.WriteString(" (")
	query.WriteString(strings.Join(quotedColumns, ", "))
	query.WriteString(") VALUES ")

	args := make([]any, 0, len(rows)*len(columns))
	parameter := int64(1)
	numberBuffer := make([]byte, 0, 8)
	for rowIndex, row := range rows {
		if len(row) != len(columns) {
			return "", nil, fmt.Errorf("row %d has %d values for %d columns", rowIndex, len(row), len(columns))
		}
		if rowIndex > 0 {
			query.WriteString(", ")
		}
		query.WriteByte('(')
		for columnIndex, value := range row {
			if columnIndex > 0 {
				query.WriteString(", ")
			}
			query.WriteByte('$')
			numberBuffer = strconv.AppendInt(numberBuffer[:0], parameter, 10)
			query.Write(numberBuffer)
			parameter++
			args = append(args, value)
		}
		query.WriteByte(')')
	}

	if len(conflictColumns) > 0 {
		quotedConflicts := make([]string, len(conflictColumns))
		for i, conflictColumn := range conflictColumns {
			if !containsIdentifier(columns, conflictColumn) {
				return "", nil, fmt.Errorf("conflict column %q is not present in the insert columns", conflictColumn)
			}
			quotedConflicts[i] = quotePostgresIdentifier(conflictColumn)
		}

		assignments := make([]string, 0, len(columns))
		for _, column := range columns {
			if containsIdentifier(conflictColumns, column) {
				continue
			}
			quotedColumn := quotePostgresIdentifier(column)
			assignments = append(assignments, fmt.Sprintf("%s = EXCLUDED.%s", quotedColumn, quotedColumn))
		}
		if len(assignments) == 0 {
			quotedColumn := quotedConflicts[0]
			assignments = append(assignments, fmt.Sprintf("%s = EXCLUDED.%s", quotedColumn, quotedColumn))
		}

		fmt.Fprintf(
			&query,
			" ON CONFLICT (%s) DO UPDATE SET %s",
			strings.Join(quotedConflicts, ", "),
			strings.Join(assignments, ", "),
		)
	}
	return query.String(), args, nil
}

func containsIdentifier(columns []string, target string) bool {
	for _, column := range columns {
		if strings.EqualFold(column, target) {
			return true
		}
	}
	return false
}

func postgresInsertChunkSize(columnCount int) int {
	if columnCount <= 0 {
		return 0
	}
	chunkSize := maxPostgresParameters / columnCount
	if chunkSize > maxRowsPerInsert {
		return maxRowsPerInsert
	}
	if chunkSize < 1 {
		return 1
	}
	return chunkSize
}
