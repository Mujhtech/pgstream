package migrator

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// columnKind is the precompiled dispatch decision for one target column.
// Classifying each column once per table replaces the per-cell string
// comparisons, map lookups, and strings.ToLower allocations the row loop
// used to perform millions of times (see hotpath_bench_test.go).
type columnKind uint8

const (
	// kindOther passes unknown types through the generic trimmed-string path.
	kindOther columnKind = iota
	kindBytea
	kindUUID
	kindBoolean
	// kindVerbatimText copies text exactly; whitespace can be meaningful.
	kindVerbatimText
	kindTime
	kindDate
	kindTimestamp
	kindInteger
	kindNumeric
	kindEnum
	// kindOtherTime covers unrecognized types whose name contains "time";
	// values validate as times, mirroring the legacy default branch.
	kindOtherTime
)

func classifyColumnKind(dataType string) columnKind {
	switch dataType {
	case "bytea":
		return kindBytea
	case "uuid":
		return kindUUID
	case "boolean":
		return kindBoolean
	case "text", "json", "jsonb":
		return kindVerbatimText
	case "time", "time without time zone", "time with time zone":
		return kindTime
	case "date":
		return kindDate
	case "timestamp", "timestamp without time zone", "timestamp with time zone":
		return kindTimestamp
	case "integer", "bigint", "smallint":
		return kindInteger
	case "numeric", "decimal", "real", "double precision":
		return kindNumeric
	}
	if strings.Contains(dataType, "character") {
		return kindVerbatimText
	}
	if strings.Contains(strings.ToLower(dataType), "time") {
		return kindOtherTime
	}
	return kindOther
}

// columnPlan is the resolved validation/transform plan for one column.
type columnPlan struct {
	name       string
	dataType   string
	kind       columnKind
	nullable   bool
	enumValues map[string]struct{}
}

// buildColumnPlans resolves target metadata for the batch's column list once,
// so the per-row loop touches no maps and allocates nothing for dispatch.
func (m *Migrator) buildColumnPlans(ctx context.Context, table string, columns []string) ([]columnPlan, error) {
	metadata, err := m.loadValidationMetadata(ctx, table)
	if err != nil {
		return nil, err
	}

	plans := make([]columnPlan, len(columns))
	for j, column := range columns {
		key := strings.ToLower(column)
		info, exists := metadata.columns[key]
		if !exists {
			return nil, fmt.Errorf("PostgreSQL column metadata not found for %s.%s", table, column)
		}
		plan := columnPlan{
			name:     info.name,
			dataType: info.dataType,
			nullable: info.isNullable,
		}
		if m.isEnumColumn(info.dataType, info.udtName) {
			plan.kind = kindEnum
			plan.enumValues = metadata.enumValuesByColumn[key]
		} else {
			plan.kind = classifyColumnKind(info.dataType)
		}
		plans[j] = plan
	}
	return plans, nil
}

// transformRows validates and converts a batch in place. Rows are freshly
// scanned per batch and owned by this pipeline stage, so mutating them avoids
// one slice allocation and copy per row.
func (m *Migrator) transformRows(table string, plans []columnPlan, data [][]any) error {
	for i, row := range data {
		if len(row) != len(plans) {
			return fmt.Errorf("row %d has %d values for %d columns", i, len(row), len(plans))
		}
		for j, value := range row {
			plan := &plans[j]
			if value == nil {
				if !plan.nullable {
					return fmt.Errorf("row %d column %s.%s is NULL but the target column is NOT NULL", i, table, plan.name)
				}
				continue
			}

			// MySQL's zero date has no PostgreSQL representation (no year
			// zero, no month/day zero). It means "no date", so nullable
			// targets take NULL; NOT NULL targets fail with guidance.
			if (plan.kind == kindDate || plan.kind == kindTimestamp) && hasZeroDatePrefix(value) {
				if !plan.nullable {
					return fmt.Errorf(
						"row %d column %s.%s holds MySQL's zero date %q, which PostgreSQL cannot represent, and the target column is NOT NULL. Fix the source rows first (find them with: SELECT * FROM %s WHERE %s = '0000-00-00 00:00:00' — or LIKE '0000-00-00%%'), either setting a real date or making the column nullable",
						i, table, plan.name, value, quoteMySQLIdentifier(table), quoteMySQLIdentifier(plan.name),
					)
				}
				m.warnZeroDate(table, plan.name)
				row[j] = nil
				continue
			}

			if plan.kind == kindEnum {
				strValue, ok := stringValue(value)
				if !ok {
					return fmt.Errorf("enum column %s.%s has unsupported value type %T", table, plan.name, value)
				}
				if _, valid := plan.enumValues[strValue]; !valid {
					return fmt.Errorf("enum column %s.%s contains invalid value %q", table, plan.name, strValue)
				}
				row[j] = strValue
				continue
			}

			transformed, err := m.transformValueKind(plan.kind, plan.dataType, value)
			if err != nil {
				return fmt.Errorf("row %d column %s.%s: %w", i, table, plan.name, err)
			}
			row[j] = transformed
		}
	}
	return nil
}

// hasZeroDatePrefix reports MySQL zero-date values ("0000-00-00", with or
// without a time part) without allocating; the driver delivers temporal
// values as []byte or string.
func hasZeroDatePrefix(value any) bool {
	const zero = "0000-00-00"
	switch v := value.(type) {
	case []byte:
		return len(v) >= len(zero) && string(v[:len(zero)]) == zero
	case string:
		return strings.HasPrefix(v, zero)
	}
	return false
}

// warnZeroDate logs the NULL conversion once per column per run.
func (m *Migrator) warnZeroDate(table, column string) {
	key := table + "\x00" + column
	m.zeroDateMu.Lock()
	if m.zeroDateWarned == nil {
		m.zeroDateWarned = make(map[string]bool)
	}
	alreadyWarned := m.zeroDateWarned[key]
	m.zeroDateWarned[key] = true
	m.zeroDateMu.Unlock()
	if !alreadyWarned {
		m.warnf("⚠️  Column %s.%s contains MySQL zero dates ('0000-00-00 ...'); PostgreSQL cannot represent them, so they migrate as NULL. Find the source rows with: SELECT * FROM %s WHERE %s LIKE '0000-00-00%%'", table, column, quoteMySQLIdentifier(table), quoteMySQLIdentifier(column))
	}
}

// transformValueKind mirrors validateAndTransformValue's semantics with the
// type dispatch resolved ahead of time. value is never nil here.
func (m *Migrator) transformValueKind(kind columnKind, dataType string, value any) (any, error) {
	switch kind {
	case kindBytea:
		return value, nil
	case kindUUID:
		if bytes, ok := value.([]byte); ok && len(bytes) == 16 {
			if parsed, err := uuid.FromBytes(bytes); err == nil {
				return parsed.String(), nil
			}
		}
	case kindBoolean:
		if booleanValue, ok := booleanValue(value); ok {
			return booleanValue, nil
		}
		return nil, fmt.Errorf("invalid boolean value %v", value)
	}

	strValue, ok := stringValue(value)
	if !ok {
		return value, nil
	}
	if kind == kindVerbatimText {
		return strValue, nil
	}

	strValue = strings.TrimSpace(strValue)
	if strValue == "" {
		return nil, fmt.Errorf("empty value cannot be represented as %s", dataType)
	}

	switch kind {
	case kindTime, kindOtherTime:
		if m.isValidTime(strValue) {
			return strValue, nil
		}
		return nil, fmt.Errorf("invalid time value %q", strValue)
	case kindDate:
		if m.isValidDate(strValue) {
			return strValue, nil
		}
		return nil, fmt.Errorf("invalid date value %q", strValue)
	case kindTimestamp:
		if m.isValidTimestamp(strValue) {
			return strValue, nil
		}
		return nil, fmt.Errorf("invalid timestamp value %q", strValue)
	case kindInteger:
		if m.isValidInteger(strValue) {
			return strValue, nil
		}
		return nil, fmt.Errorf("invalid integer value %q", strValue)
	case kindNumeric:
		if m.isValidNumeric(strValue) {
			return strValue, nil
		}
		return nil, fmt.Errorf("invalid numeric value %q", strValue)
	case kindUUID:
		parsed, err := uuid.Parse(strValue)
		if err != nil {
			return nil, fmt.Errorf("invalid UUID value %q: %w", strValue, err)
		}
		return parsed.String(), nil
	}

	return strValue, nil
}

// validateAndTransformData validates a batch and converts values in place.
// The heavy lifting lives in buildColumnPlans/transformRows: the plan
// resolves all per-column metadata once, so the per-cell loop performs no
// map lookups, no case folding, and no string-based type dispatch
// (benchmarked in hotpath_bench_test.go).
func (m *Migrator) validateAndTransformData(ctx context.Context, table string, columns []string, data [][]any) ([][]any, error) {
	plans, err := m.buildColumnPlans(ctx, table, columns)
	if err != nil {
		return nil, err
	}
	if err := m.transformRows(table, plans, data); err != nil {
		return nil, err
	}
	return data, nil
}

func stringValue(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case []byte:
		return string(value), true
	default:
		return "", false
	}
}

func (m *Migrator) validateAndTransformValue(value any, dataType string) (any, error) {
	if value == nil {
		return nil, nil
	}

	if dataType == "bytea" {
		return value, nil
	}
	if dataType == "uuid" {
		if bytes, ok := value.([]byte); ok && len(bytes) == 16 {
			if parsed, err := uuid.FromBytes(bytes); err == nil {
				return parsed.String(), nil
			}
		}
	}
	if dataType == "boolean" {
		if booleanValue, ok := booleanValue(value); ok {
			return booleanValue, nil
		}
		return nil, fmt.Errorf("invalid boolean value %v", value)
	}

	strValue, ok := stringValue(value)
	if !ok {
		return value, nil
	}

	// Text is copied exactly. Whitespace can be meaningful data.
	if dataType == "text" || strings.Contains(dataType, "character") || dataType == "json" || dataType == "jsonb" {
		return strValue, nil
	}

	strValue = strings.TrimSpace(strValue)
	if strValue == "" {
		return nil, fmt.Errorf("empty value cannot be represented as %s", dataType)
	}

	switch dataType {
	case "time", "time without time zone", "time with time zone":
		if m.isValidTime(strValue) {
			return strValue, nil
		}
		return nil, fmt.Errorf("invalid time value %q", strValue)
	case "date":
		if m.isValidDate(strValue) {
			return strValue, nil
		}
		return nil, fmt.Errorf("invalid date value %q", strValue)
	case "timestamp", "timestamp without time zone", "timestamp with time zone":
		if m.isValidTimestamp(strValue) {
			return strValue, nil
		}
		return nil, fmt.Errorf("invalid timestamp value %q", strValue)
	case "integer", "bigint", "smallint":
		if m.isValidInteger(strValue) {
			return strValue, nil
		}
		return nil, fmt.Errorf("invalid integer value %q", strValue)
	case "numeric", "decimal", "real", "double precision":
		if m.isValidNumeric(strValue) {
			return strValue, nil
		}
		return nil, fmt.Errorf("invalid numeric value %q", strValue)
	case "boolean":
		if parsed, ok := booleanStringValue(strValue); ok {
			return parsed, nil
		}
		return nil, fmt.Errorf("invalid boolean value %q", strValue)
	case "uuid":
		parsed, err := uuid.Parse(strValue)
		if err != nil {
			return nil, fmt.Errorf("invalid UUID value %q: %w", strValue, err)
		}
		return parsed.String(), nil
	default:
		if strings.Contains(strings.ToLower(dataType), "time") {
			if !m.isValidTime(strValue) {
				return nil, fmt.Errorf("invalid time value %q for type %s", strValue, dataType)
			}
		}
	}

	return strValue, nil
}

func booleanValue(value any) (bool, bool) {
	switch value := value.(type) {
	case bool:
		return value, true
	case int:
		return numericBoolean(int64(value))
	case int8:
		return numericBoolean(int64(value))
	case int16:
		return numericBoolean(int64(value))
	case int32:
		return numericBoolean(int64(value))
	case int64:
		return numericBoolean(value)
	case uint:
		return unsignedBoolean(uint64(value))
	case uint8:
		return unsignedBoolean(uint64(value))
	case uint16:
		return unsignedBoolean(uint64(value))
	case uint32:
		return unsignedBoolean(uint64(value))
	case uint64:
		return unsignedBoolean(value)
	case []byte:
		if len(value) == 1 && (value[0] == 0 || value[0] == 1) {
			return value[0] == 1, true
		}
		return booleanStringValue(string(value))
	case string:
		return booleanStringValue(value)
	default:
		return false, false
	}
}

func numericBoolean(value int64) (bool, bool) {
	if value == 0 || value == 1 {
		return value == 1, true
	}
	return false, false
}

func unsignedBoolean(value uint64) (bool, bool) {
	if value == 0 || value == 1 {
		return value == 1, true
	}
	return false, false
}

func booleanStringValue(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "t", "y":
		return true, true
	case "false", "0", "no", "f", "n":
		return false, true
	default:
		return false, false
	}
}

func (m *Migrator) isValidTime(value string) bool {
	timeFormats := []string{
		"15:04:05",
		"15:04",
	}

	for _, format := range timeFormats {
		if _, err := time.Parse(format, value); err == nil {
			return true
		}
	}

	return false
}

func (m *Migrator) isValidDate(value string) bool {
	_, err := time.Parse("2006-01-02", value)
	return err == nil
}

func (m *Migrator) isValidTimestamp(value string) bool {
	// Check if it's a valid timestamp format
	timestampFormats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	}

	for _, format := range timestampFormats {
		if _, err := time.Parse(format, value); err == nil {
			return true
		}
	}

	return false
}

func (m *Migrator) isValidInteger(value string) bool {
	// Remove any leading/trailing whitespace
	value = strings.TrimSpace(value)

	// Check if it's a valid integer
	if value == "" {
		return false
	}

	// Check for valid integer patterns
	if _, err := strconv.ParseInt(value, 10, 64); err == nil {
		return true
	}

	return false
}

func (m *Migrator) isValidNumeric(value string) bool {
	// Remove any leading/trailing whitespace
	value = strings.TrimSpace(value)

	// Check if it's a valid numeric
	if value == "" {
		return false
	}

	// Check for valid numeric patterns
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return true
	}

	return false
}
