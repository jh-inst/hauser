package testing

import (
	"context"
	"fmt"
	"time"

	"github.com/fullstorydev/hauser/warehouse"
)

var (
	MockTypeMap = warehouse.FieldTypeMapper{
		"int64":     "BIGINT",
		"string":    "VARCHAR(max)",
		"time.Time": "TIMESTAMP",
	}
)

type MockDatabase struct {
	schema      warehouse.Schema
	Initialized bool
	Syncs       []time.Time
	LoadedFiles []string
}

var _ warehouse.Database = (*MockDatabase)(nil)

func NewMockDatabase() *MockDatabase {
	return &MockDatabase{
		schema:      nil,
		Initialized: false,
		Syncs:       nil,
		LoadedFiles: nil,
	}
}

func (m *MockDatabase) LastSyncPoint(_ context.Context) (time.Time, error) {
	var max time.Time
	for i, s := range m.Syncs {
		if i == 0 || s.After(max) {
			max = s
		}
	}
	return max, nil
}

func (m *MockDatabase) SaveSyncPoint(_ context.Context, endTime time.Time) error {
	m.Syncs = append(m.Syncs, endTime)
	return nil
}

func (m *MockDatabase) LoadToWarehouse(filename string, _ time.Time) error {
	m.LoadedFiles = append(m.LoadedFiles, filename)
	return nil
}

func (m *MockDatabase) ValueToString(val interface{}, isTime bool) string {
	s := fmt.Sprintf("%v", val)
	if isTime {
		t, _ := time.Parse(time.RFC3339Nano, s)
		return t.Format(warehouse.RFC3339Micro)
	}
	return s
}

func (m *MockDatabase) GetExportTableColumns() []string {
	cols := make([]string, 0, len(m.schema))
	for _, f := range m.schema {
		cols = append(cols, f.Name)
	}
	return cols
}

func (m *MockDatabase) EnsureCompatibleExportTable() error {
	m.schema = warehouse.ExportTableSchema(MockTypeMap)
	m.Initialized = true
	return nil
}
