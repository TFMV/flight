package flight

import (
	"database/sql"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/flight/flightsql"

	"github.com/apache/arrow-go/v18/arrow/memory"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func getArrowTypeFromString(dbtype string) arrow.DataType {
	dbtype = strings.ToLower(dbtype)
	if dbtype == "" {
		// DuckDB may not know the type yet.
		return &arrow.NullType{}
	}
	if strings.HasPrefix(dbtype, "varchar") {
		return arrow.BinaryTypes.String
	}

	switch dbtype {
	case "tinyint":
		return arrow.PrimitiveTypes.Int8
	case "smallint":
		return arrow.PrimitiveTypes.Int16
	case "integer", "int", "int32":
		return arrow.PrimitiveTypes.Int32
	case "bigint", "int64":
		return arrow.PrimitiveTypes.Int64
	case "float":
		return arrow.PrimitiveTypes.Float32
	case "double":
		return arrow.PrimitiveTypes.Float64
	case "blob":
		return arrow.BinaryTypes.Binary
	case "text", "varchar", "string":
		return arrow.BinaryTypes.String
	case "date":
		return arrow.FixedWidthTypes.Date32
	case "time":
		return arrow.FixedWidthTypes.Time32s
	case "timestamp":
		return arrow.FixedWidthTypes.Timestamp_us
	case "boolean":
		return arrow.FixedWidthTypes.Boolean
	default:
		panic("invalid DuckDB type: " + dbtype)
	}
}

var duckDBDenseUnion = arrow.DenseUnionOf([]arrow.Field{
	{Name: "int", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	{Name: "float", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
	{Name: "string", Type: arrow.BinaryTypes.String, Nullable: true},
}, []arrow.UnionTypeCode{0, 1, 2})

func getArrowType(c *sql.ColumnType) arrow.DataType {
	dbtype := strings.ToLower(c.DatabaseTypeName())
	if dbtype == "" {
		if c.ScanType() == nil {
			return duckDBDenseUnion
		}
		switch c.ScanType().Kind() {
		case reflect.Int8, reflect.Uint8:
			return arrow.PrimitiveTypes.Int8
		case reflect.Int16, reflect.Uint16:
			return arrow.PrimitiveTypes.Int16
		case reflect.Int32, reflect.Uint32:
			return arrow.PrimitiveTypes.Int32
		case reflect.Int, reflect.Int64, reflect.Uint64:
			return arrow.PrimitiveTypes.Int64
		case reflect.Float32:
			return arrow.PrimitiveTypes.Float32
		case reflect.Float64:
			return arrow.PrimitiveTypes.Float64
		case reflect.String:
			return arrow.BinaryTypes.String
		case reflect.Bool:
			return arrow.FixedWidthTypes.Boolean
		}
	}
	return getArrowTypeFromString(dbtype)
}

const maxBatchSize = 1024

type SqlBatchReader struct {
	refCount int64

	schema *arrow.Schema
	rows   *sql.Rows
	record arrow.Record
	bldr   *array.RecordBuilder
	err    error

	rowdest []interface{}
}

func NewDuckDBBatchReaderWithSchema(mem memory.Allocator, schema *arrow.Schema, rows *sql.Rows) (*SqlBatchReader, error) {
	rowdest := make([]interface{}, schema.NumFields())
	for i, f := range schema.Fields() {
		switch f.Type.ID() {
		case arrow.DENSE_UNION, arrow.SPARSE_UNION:
			rowdest[i] = new(interface{})
		case arrow.UINT8, arrow.INT8:
			if f.Nullable {
				rowdest[i] = &sql.NullByte{}
			} else {
				rowdest[i] = new(uint8)
			}
		case arrow.INT32:
			if f.Nullable {
				rowdest[i] = &sql.NullInt32{}
			} else {
				rowdest[i] = new(int32)
			}
		case arrow.INT64:
			if f.Nullable {
				rowdest[i] = &sql.NullInt64{}
			} else {
				rowdest[i] = new(int64)
			}
		case arrow.FLOAT32, arrow.FLOAT64:
			if f.Nullable {
				rowdest[i] = &sql.NullFloat64{}
			} else {
				rowdest[i] = new(float64)
			}
		case arrow.BINARY:
			var b []byte
			rowdest[i] = &b
		case arrow.STRING:
			if f.Nullable {
				rowdest[i] = &sql.NullString{}
			} else {
				rowdest[i] = new(string)
			}
		}
	}

	return &SqlBatchReader{
		refCount: 1,
		bldr:     array.NewRecordBuilder(mem, schema),
		schema:   schema,
		rowdest:  rowdest,
		rows:     rows}, nil
}

func NewDuckDBBatchReader(mem memory.Allocator, rows *sql.Rows) (*SqlBatchReader, error) {
	bldr := flightsql.NewColumnMetadataBuilder()

	cols, err := rows.ColumnTypes()
	if err != nil {
		rows.Close()
		return nil, err
	}

	rowdest := make([]interface{}, len(cols))
	fields := make([]arrow.Field, len(cols))
	for i, c := range cols {
		fields[i].Name = c.Name()
		if c.Name() == "?" {
			fields[i].Name += ":" + strconv.Itoa(i)
		}
		fields[i].Nullable, _ = c.Nullable()
		fields[i].Type = getArrowType(c)
		fields[i].Metadata = getColumnMetadata(bldr, getSqlTypeFromTypeName(c.DatabaseTypeName()), "")
		switch fields[i].Type.ID() {
		case arrow.DENSE_UNION, arrow.SPARSE_UNION:
			rowdest[i] = new(interface{})
		case arrow.UINT8, arrow.INT8:
			if fields[i].Nullable {
				rowdest[i] = &sql.NullByte{}
			} else {
				rowdest[i] = new(uint8)
			}
		case arrow.INT32:
			if fields[i].Nullable {
				rowdest[i] = &sql.NullInt32{}
			} else {
				rowdest[i] = new(int32)
			}
		case arrow.INT64:
			if fields[i].Nullable {
				rowdest[i] = &sql.NullInt64{}
			} else {
				rowdest[i] = new(int64)
			}
		case arrow.FLOAT64, arrow.FLOAT32:
			if fields[i].Nullable {
				rowdest[i] = &sql.NullFloat64{}
			} else {
				rowdest[i] = new(float64)
			}
		case arrow.BINARY:
			var b []byte
			rowdest[i] = &b
		case arrow.STRING:
			if fields[i].Nullable {
				rowdest[i] = &sql.NullString{}
			} else {
				rowdest[i] = new(string)
			}
		}
	}

	schema := arrow.NewSchema(fields, nil)
	return &SqlBatchReader{
		refCount: 1,
		bldr:     array.NewRecordBuilder(mem, schema),
		schema:   schema,
		rowdest:  rowdest,
		rows:     rows}, nil
}

func (r *SqlBatchReader) Retain() {
	atomic.AddInt64(&r.refCount, 1)
}

func (r *SqlBatchReader) Release() {

	if atomic.AddInt64(&r.refCount, -1) == 0 {
		r.rows.Close()
		r.rows, r.schema, r.rowdest = nil, nil, nil
		r.bldr.Release()
		r.bldr = nil
		if r.record != nil {
			r.record.Release()
			r.record = nil
		}
	}
}
func (r *SqlBatchReader) Schema() *arrow.Schema { return r.schema }

func (r *SqlBatchReader) Record() arrow.Record { return r.record }

func (r *SqlBatchReader) Err() error { return r.err }

func (r *SqlBatchReader) Next() bool {
	if r.record != nil {
		r.record.Release()
		r.record = nil
	}

	rows := 0
	for rows < maxBatchSize && r.rows.Next() {
		if err := r.rows.Scan(r.rowdest...); err != nil {
			// Not really useful except for testing Flight SQL clients
			detail := wrapperspb.StringValue{Value: r.schema.String()}
			if st, sterr := status.New(codes.Unknown, err.Error()).WithDetails(&detail); sterr != nil {
				r.err = err
			} else {
				r.err = st.Err()
			}
			return false
		}

		for i, v := range r.rowdest {
			fb := r.bldr.Field(i)

			switch v := v.(type) {
			case *uint8:
				fb.(*array.Uint8Builder).Append(*v)
			case *sql.NullByte:
				if !v.Valid {
					fb.AppendNull()
				} else {
					fb.(*array.Uint8Builder).Append(v.Byte)
				}
			case *int64:
				fb.(*array.Int64Builder).Append(*v)
			case *sql.NullInt64:
				if !v.Valid {
					fb.AppendNull()
				} else {
					fb.(*array.Int64Builder).Append(v.Int64)
				}
			case *int32:
				fb.(*array.Int32Builder).Append(*v)
			case *sql.NullInt32:
				if !v.Valid {
					fb.AppendNull()
				} else {
					fb.(*array.Int32Builder).Append(v.Int32)
				}
			case *float64:
				switch b := fb.(type) {
				case *array.Float64Builder:
					b.Append(*v)
				case *array.Float32Builder:
					b.Append(float32(*v))
				}
			case *sql.NullFloat64:
				if !v.Valid {
					fb.AppendNull()
				} else {
					switch b := fb.(type) {
					case *array.Float64Builder:
						b.Append(v.Float64)
					case *array.Float32Builder:
						b.Append(float32(v.Float64))
					}
				}
			case *[]byte:
				if v == nil {
					fb.AppendNull()
				} else {
					fb.(*array.BinaryBuilder).Append(*v)
				}
			case *string:
				fb.(*array.StringBuilder).Append(*v)
			case *sql.NullString:
				if !v.Valid {
					fb.AppendNull()
				} else {
					fb.(*array.StringBuilder).Append(v.String)
				}
			}
		}

		rows++
	}

	r.record = r.bldr.NewRecord()
	return rows > 0
}
