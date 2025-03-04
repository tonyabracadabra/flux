package testing

import (
	"math"
	"sort"
	"sync"

	"github.com/influxdata/flux"
	"github.com/influxdata/flux/array"
	"github.com/influxdata/flux/arrow"
	"github.com/influxdata/flux/codes"
	"github.com/influxdata/flux/execute"
	"github.com/influxdata/flux/internal/errors"
	"github.com/influxdata/flux/memory"
	"github.com/influxdata/flux/plan"
	"github.com/influxdata/flux/runtime"
)

const DiffKind = "diff"
const DefaultEpsilon = 1e-6
const DefaultNaNsEqual = false

type DiffOpSpec struct {
	Verbose   bool    `json:"verbose,omitempty"`
	Epsilon   float64 `json:"epsilon"`
	NaNsEqual bool    `json:"nansEqual,omitempty"`
}

func (s *DiffOpSpec) Kind() flux.OperationKind {
	return DiffKind
}

func init() {
	diffSignature := runtime.MustLookupBuiltinType("testing", "diff")

	runtime.RegisterPackageValue("testing", "diff", flux.MustValue(flux.FunctionValue(DiffKind, createDiffOpSpec, diffSignature)))
	flux.RegisterOpSpec(DiffKind, newDiffOp)
	plan.RegisterProcedureSpec(DiffKind, newDiffProcedure, DiffKind)
	execute.RegisterTransformation(DiffKind, createDiffTransformation)
}

func createDiffOpSpec(args flux.Arguments, a *flux.Administration) (flux.OperationSpec, error) {
	t, ok := args.Get("want")
	if !ok {
		return nil, errors.New(codes.Invalid, "argument 'want' not present")
	}
	p, ok := t.(*flux.TableObject)
	if !ok {
		return nil, errors.New(codes.Invalid, "want input to diff is not a table object")
	}
	a.AddParent(p)

	t, ok = args.Get("got")
	if !ok {
		return nil, errors.New(codes.Invalid, "argument 'got' not present")
	}
	p, ok = t.(*flux.TableObject)
	if !ok {
		return nil, errors.New(codes.Invalid, "got input to diff is not a table object")
	}
	a.AddParent(p)

	verbose, ok, err := args.GetBool("verbose")
	if err != nil {
		return nil, err
	} else if !ok {
		verbose = false
	}

	epsilon, ok, err := args.GetFloat("epsilon")
	if err != nil {
		return nil, err
	} else if !ok {
		epsilon = DefaultEpsilon
	}
	nansEqual, ok, err := args.GetBool("nansEqual")
	if err != nil {
		return nil, err
	} else if !ok {
		nansEqual = DefaultNaNsEqual
	}

	return &DiffOpSpec{Verbose: verbose, Epsilon: epsilon, NaNsEqual: nansEqual}, nil
}

func newDiffOp() flux.OperationSpec {
	return new(DiffOpSpec)
}

type DiffProcedureSpec struct {
	plan.DefaultCost
	Verbose bool
	Epsilon float64
}

func (s *DiffProcedureSpec) Kind() plan.ProcedureKind {
	return DiffKind
}

func (s *DiffProcedureSpec) Copy() plan.ProcedureSpec {
	ns := *s
	return &ns
}

func newDiffProcedure(qs flux.OperationSpec, pa plan.Administration) (plan.ProcedureSpec, error) {
	spec, ok := qs.(*DiffOpSpec)
	if !ok {
		return nil, errors.Newf(codes.Internal, "invalid spec type %T", qs)
	}
	return &DiffProcedureSpec{Verbose: spec.Verbose, Epsilon: spec.Epsilon}, nil
}

type DiffTransformation struct {
	execute.ExecutionNode
	mu sync.Mutex

	wantID, gotID execute.DatasetID
	parentState   map[execute.DatasetID]*diffParentState

	d     execute.Dataset
	cache execute.TableBuilderCache
	alloc *memory.Allocator

	inputCache *execute.RandomAccessGroupLookup

	epsilon   float64
	nansEqual bool
}

type diffParentState struct {
	mark       execute.Time
	processing execute.Time
	finished   bool
}

type tableBuffer struct {
	id      execute.DatasetID
	columns map[string]*tableColumn
	sz      int
}

func (tb *tableBuffer) Release() {
	for _, col := range tb.columns {
		col.Values.Release()
	}
}

type tableColumn struct {
	Type   flux.ColType
	Values array.Array
}

func copyTable(id execute.DatasetID, tbl flux.Table, alloc *memory.Allocator) (*tableBuffer, error) {
	// Find the value columns for the table and save them.
	// We do not care about the group key.
	type tableBuilderColumn struct {
		Type    flux.ColType
		Builder array.Builder
	}
	builders := make(map[string]tableBuilderColumn)
	for _, col := range tbl.Cols() {
		if tbl.Key().HasCol(col.Label) {
			continue
		}

		bc := tableBuilderColumn{Type: col.Type}
		switch col.Type {
		case flux.TFloat:
			bc.Builder = arrow.NewFloatBuilder(alloc)
		case flux.TInt:
			bc.Builder = arrow.NewIntBuilder(alloc)
		case flux.TUInt:
			bc.Builder = arrow.NewUintBuilder(alloc)
		case flux.TString:
			bc.Builder = arrow.NewStringBuilder(alloc)
		case flux.TBool:
			bc.Builder = arrow.NewBoolBuilder(alloc)
		case flux.TTime:
			bc.Builder = arrow.NewIntBuilder(alloc)
		default:
			return nil, errors.New(codes.Unimplemented)
		}
		builders[col.Label] = bc
	}

	sz := 0
	if err := tbl.Do(func(cr flux.ColReader) error {
		sz += cr.Len()
		for j, col := range cr.Cols() {
			if tbl.Key().HasCol(col.Label) {
				continue
			}

			switch col.Type {
			case flux.TFloat:
				b := builders[col.Label].Builder.(*array.FloatBuilder)
				b.Reserve(cr.Len())

				vs := cr.Floats(j)
				for i := 0; i < vs.Len(); i++ {
					if vs.IsValid(i) {
						b.Append(vs.Value(i))
					} else {
						b.AppendNull()
					}
				}
			case flux.TInt:
				b := builders[col.Label].Builder.(*array.IntBuilder)
				b.Reserve(cr.Len())

				vs := cr.Ints(j)
				for i := 0; i < vs.Len(); i++ {
					if vs.IsValid(i) {
						b.Append(vs.Value(i))
					} else {
						b.AppendNull()
					}
				}
			case flux.TUInt:
				b := builders[col.Label].Builder.(*array.UintBuilder)
				b.Reserve(cr.Len())

				vs := cr.UInts(j)
				for i := 0; i < vs.Len(); i++ {
					if vs.IsValid(i) {
						b.Append(vs.Value(i))
					} else {
						b.AppendNull()
					}
				}
			case flux.TString:
				b := builders[col.Label].Builder.(*array.StringBuilder)
				b.Reserve(cr.Len())

				vs := cr.Strings(j)
				for i := 0; i < vs.Len(); i++ {
					if vs.IsValid(i) {
						b.Append(vs.Value(i))
					} else {
						b.AppendNull()
					}
				}
			case flux.TBool:
				b := builders[col.Label].Builder.(*array.BooleanBuilder)
				b.Reserve(cr.Len())

				vs := cr.Bools(j)
				for i := 0; i < vs.Len(); i++ {
					if vs.IsValid(i) {
						b.Append(vs.Value(i))
					} else {
						b.AppendNull()
					}
				}
			case flux.TTime:
				b := builders[col.Label].Builder.(*array.IntBuilder)
				b.Reserve(cr.Len())

				vs := cr.Times(j)
				for i := 0; i < vs.Len(); i++ {
					if vs.IsValid(i) {
						b.Append(vs.Value(i))
					} else {
						b.AppendNull()
					}
				}
			default:
				return errors.New(codes.Unimplemented)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// Construct each of the columns and then store the table buffer.
	columns := make(map[string]*tableColumn, len(builders))
	for label, bc := range builders {
		columns[label] = &tableColumn{
			Type:   bc.Type,
			Values: bc.Builder.NewArray(),
		}
		bc.Builder.Release()
	}
	return &tableBuffer{
		id:      id,
		columns: columns,
		sz:      sz,
	}, nil
}

func createDiffTransformation(id execute.DatasetID, mode execute.AccumulationMode, spec plan.ProcedureSpec, a execute.Administration) (execute.Transformation, execute.Dataset, error) {
	if len(a.Parents()) != 2 {
		return nil, nil, errors.New(codes.Internal, "diff should have exactly 2 parents")
	}

	cache := execute.NewTableBuilderCache(a.Allocator())
	dataset := execute.NewDataset(id, mode, cache)
	pspec, ok := spec.(*DiffProcedureSpec)
	if !ok {
		return nil, nil, errors.Newf(codes.Internal, "invalid spec type %T", pspec)
	}

	transform := NewDiffTransformation(dataset, cache, pspec, a.Parents()[0], a.Parents()[1], a.Allocator())

	return transform, dataset, nil
}

func NewDiffTransformation(d execute.Dataset, cache execute.TableBuilderCache, spec *DiffProcedureSpec, wantID, gotID execute.DatasetID, a *memory.Allocator) *DiffTransformation {
	parentState := make(map[execute.DatasetID]*diffParentState)
	parentState[wantID] = new(diffParentState)
	parentState[gotID] = new(diffParentState)
	return &DiffTransformation{
		wantID:      wantID,
		gotID:       gotID,
		d:           d,
		cache:       cache,
		inputCache:  execute.NewRandomAccessGroupLookup(),
		parentState: parentState,
		alloc:       a,
		epsilon:     spec.Epsilon,
	}
}

func (t *DiffTransformation) RetractTable(id execute.DatasetID, key flux.GroupKey) error {
	panic("implement me")
}

func (t *DiffTransformation) Process(id execute.DatasetID, tbl flux.Table) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// If one of the tables finished with an error, it is possible
	// to prematurely declare the other table as finished so we
	// don't do more work on something that failed anyway.
	if t.parentState[id].finished {
		tbl.Done()
		return nil
	}

	// Copy the table we are processing into a buffer.
	// This may or may not be the want table. We fix that later.
	want, err := copyTable(id, tbl, t.alloc)
	if err != nil {
		return err
	}

	// Look in the input cache for a table buffer.
	var got *tableBuffer
	if obj, ok := t.inputCache.Delete(tbl.Key()); !ok {
		// We did not find an entry. If the other table has
		// not been finished, we need to store this table
		// for later usage.
		if !t.parentState[id].finished {
			t.inputCache.Set(tbl.Key(), want)
			return nil
		}

		// The other table has been finished so we can construct
		// this table immediately. Generate an empty table buffer.
		got = &tableBuffer{}
	} else {
		// Otherwise, we assign the stored table buffer to got
		// so we can generate the diff.
		got = obj.(*tableBuffer)
	}

	// If the want table does not match the want id, we need to swap
	// the tables. We use want here instead of got because goot
	// may be a pseudo-table we created above and we only need to
	// test one of them.
	if want.id != t.wantID {
		got, want = want, got
	}
	return t.diff(tbl.Key(), want, got)
}

func (t *DiffTransformation) createSchema(builder execute.TableBuilder, want, got *tableBuffer) (diffIdx int, colMap map[string]int, err error) {
	// Construct the table schema by adding columns for the table key
	// (which, by definition, cannot be different at this point),
	// a _diff column for the marker, and then the columns  for each
	// of the value types in alphabetical order.
	if err := execute.AddTableKeyCols(builder.Key(), builder); err != nil {
		return 0, nil, err
	}
	diffIdx, err = builder.AddCol(flux.ColMeta{
		Label: "_diff",
		Type:  flux.TString,
	})
	if err != nil {
		return 0, nil, err
	}

	// Determine all of the column names and their types.
	colTypes := make(map[string]flux.ColType)
	for label, col := range want.columns {
		colTypes[label] = col.Type
	}
	for label, col := range got.columns {
		if typ, ok := colTypes[label]; ok && typ != col.Type {
			return 0, nil, errors.Newf(codes.FailedPrecondition, "column types differ: want=%s got=%s", typ, col.Type)
		} else if !ok {
			colTypes[label] = col.Type
		}
	}

	labels := make([]string, 0, len(colTypes))
	for label := range colTypes {
		labels = append(labels, label)
	}
	sort.Strings(labels)

	// Now construct the schema and mark the column ids.
	colMap = make(map[string]int)
	for _, label := range labels {
		idx, err := builder.AddCol(flux.ColMeta{
			Label: label,
			Type:  colTypes[label],
		})
		if err != nil {
			return 0, nil, err
		}
		colMap[label] = idx
	}
	return diffIdx, colMap, nil
}

func (t *DiffTransformation) diff(key flux.GroupKey, want, got *tableBuffer) error {
	defer want.Release()
	defer got.Release()

	// Find the smallest size for the tables. We will only iterate
	// over these rows.
	sz := want.sz
	if got.sz < sz {
		sz = got.sz
	}

	// Look for the first row that is unequal. This is only needed
	// if the sizes are the same.
	i := 0
	if want.sz == got.sz {
		for ; i < sz; i++ {
			if eq := t.rowEqual(want, got, i); !eq {
				break
			}
		}

		// The tables are equal.
		if i == sz {
			return nil
		}
	}

	// This diff algorithm is not really a smart diff. We may want to
	// fix that in the future and we reserve the right to do that, but
	// this will just check the first row of one table with the first
	// row of the other.
	// First, construct an output table.
	builder, created := t.cache.TableBuilder(key)
	if !created {
		return errors.New(codes.FailedPrecondition, "duplicate table key")
	}

	diffIdx, columnIdxs, err := t.createSchema(builder, want, got)
	if err != nil {
		return err
	}

	for ; i < sz; i++ {
		if eq := t.rowEqual(want, got, i); !eq {
			if err := t.appendRow(builder, i, diffIdx, "-", want, columnIdxs); err != nil {
				return err
			}
			if err := t.appendRow(builder, i, diffIdx, "+", got, columnIdxs); err != nil {
				return err
			}
		}
	}

	// Append the remainder of the rows.
	for i := sz; i < want.sz; i++ {
		if err := t.appendRow(builder, i, diffIdx, "-", want, columnIdxs); err != nil {
			return err
		}
	}
	for i := sz; i < got.sz; i++ {
		if err := t.appendRow(builder, i, diffIdx, "+", got, columnIdxs); err != nil {
			return err
		}
	}
	return nil
}

func (t *DiffTransformation) rowEqual(want, got *tableBuffer, i int) bool {
	if len(want.columns) != len(got.columns) {
		return false
	}

	for label, wantCol := range want.columns {
		gotCol, ok := got.columns[label]
		if !ok {
			return false
		}

		if wantCol.Values.IsValid(i) != gotCol.Values.IsValid(i) {
			return false
		} else if wantCol.Values.IsNull(i) {
			continue
		}

		switch wantCol.Type {
		case flux.TFloat:
			want, got := wantCol.Values.(*array.Float).Value(i), gotCol.Values.(*array.Float).Value(i)
			if t.nansEqual && math.IsNaN(want) && math.IsNaN(got) {
				// treat NaNs as equal so go to next column
				continue
			}
			if math.Abs(want-got) > t.epsilon {
				return false
			}
		case flux.TInt:
			want, got := wantCol.Values.(*array.Int), gotCol.Values.(*array.Int)
			if want.Value(i) != got.Value(i) {
				return false
			}
		case flux.TUInt:
			want, got := wantCol.Values.(*array.Uint), gotCol.Values.(*array.Uint)
			if want.Value(i) != got.Value(i) {
				return false
			}
		case flux.TString:
			want, got := wantCol.Values.(*array.String), gotCol.Values.(*array.String)
			if want.Value(i) != got.Value(i) {
				return false
			}
		case flux.TBool:
			want, got := wantCol.Values.(*array.Boolean), gotCol.Values.(*array.Boolean)
			if want.Value(i) != got.Value(i) {
				return false
			}
		case flux.TTime:
			want, got := wantCol.Values.(*array.Int), gotCol.Values.(*array.Int)
			if want.Value(i) != got.Value(i) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (t *DiffTransformation) appendRow(builder execute.TableBuilder, i, diffIdx int, diff string, tbl *tableBuffer, colMap map[string]int) error {
	// Add the want column first.
	if err := execute.AppendKeyValues(builder.Key(), builder); err != nil {
		return err
	}
	// Add the diff column.
	if err := builder.AppendString(diffIdx, diff); err != nil {
		return err
	}
	// Add all of the values.
	for label, j := range colMap {
		col, ok := tbl.columns[label]
		if !ok || col.Values.IsNull(i) {
			if err := builder.AppendNil(j); err != nil {
				return err
			}
			continue
		}

		switch col.Type {
		case flux.TFloat:
			vs := col.Values.(*array.Float)
			if err := builder.AppendFloat(j, vs.Value(i)); err != nil {
				return err
			}
		case flux.TInt:
			vs := col.Values.(*array.Int)
			if err := builder.AppendInt(j, vs.Value(i)); err != nil {
				return err
			}
		case flux.TUInt:
			vs := col.Values.(*array.Uint)
			if err := builder.AppendUInt(j, vs.Value(i)); err != nil {
				return err
			}
		case flux.TString:
			vs := col.Values.(*array.String)
			if err := builder.AppendString(j, vs.Value(i)); err != nil {
				return err
			}
		case flux.TBool:
			vs := col.Values.(*array.Boolean)
			if err := builder.AppendBool(j, vs.Value(i)); err != nil {
				return err
			}
		case flux.TTime:
			vs := col.Values.(*array.Int)
			if err := builder.AppendTime(j, execute.Time(vs.Value(i))); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *DiffTransformation) UpdateWatermark(id execute.DatasetID, mark execute.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.parentState[id].mark = mark

	min := execute.Time(math.MaxInt64)
	for _, state := range t.parentState {
		if state.mark < min {
			min = state.mark
		}
	}

	return t.d.UpdateWatermark(min)
}

func (t *DiffTransformation) UpdateProcessingTime(id execute.DatasetID, mark execute.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.parentState[id].processing = mark

	min := execute.Time(math.MaxInt64)
	for _, state := range t.parentState {
		if state.processing < min {
			min = state.processing
		}
	}

	return t.d.UpdateProcessingTime(min)
}

func (t *DiffTransformation) Finish(id execute.DatasetID, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.parentState[id].finished = true

	if err != nil {
		t.d.Finish(err)
	}

	finished := true
	for _, state := range t.parentState {
		finished = finished && state.finished
	}

	if finished {
		// There will be no more tables so any tables we have should
		// have a table created with a diff for every line since all
		// of them are missing.
		err = t.inputCache.Range(func(key flux.GroupKey, value interface{}) error {
			var got, want *tableBuffer
			if obj := value.(*tableBuffer); obj.id == t.wantID {
				want, got = obj, &tableBuffer{}
			} else {
				want, got = &tableBuffer{}, obj
			}
			return t.diff(key, want, got)
		})
		t.d.Finish(err)
	}
}
