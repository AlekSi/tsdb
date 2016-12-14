package tsdb

import (
	"fmt"
	"strings"

	"github.com/fabxc/tsdb/chunks"
)

// Matcher matches a string.
type Matcher interface {
	Name() string
	// Match returns true if the matcher applies to the string value.
	Match(v string) bool
}

type equalMatcher struct {
	name  string
	value string
}

func MatchEquals(n, v string) Matcher {
	return &equalMatcher{name: n, value: v}
}

func (m *equalMatcher) Name() string        { return m.name }
func (m *equalMatcher) Match(v string) bool { return v == m.value }

// Querier provides querying access over time series data of a fixed
// time range.
type Querier interface {
	// Select returns a set of series that matches the given label matchers.
	Select(...Matcher) SeriesSet

	// LabelValues returns all potential values for a label name.
	LabelValues(string) ([]string, error)
	// LabelValuesFor returns all potential values for a label name.
	// under the constraint of another label.
	LabelValuesFor(string, Label) ([]string, error)

	// Close releases the resources of the Querier.
	Close() error
}

// Series represents a single time series.
type Series interface {
	// Labels returns the complete set of labels identifying the series.
	Labels() Labels
	// Iterator returns a new iterator of the data of the series.
	Iterator() SeriesIterator

	// Ref() uint32
}

func inRange(x, mint, maxt int64) bool {
	return x >= mint && x <= maxt
}

// querier merges query results from a set of shard querieres.
type querier struct {
	mint, maxt int64
	shards     []Querier
}

// Querier returns a new querier over the database for the given
// time range.
func (db *DB) Querier(mint, maxt int64) Querier {
	q := &querier{
		mint: mint,
		maxt: maxt,
	}
	for _, s := range db.shards {
		q.shards = append(q.shards, s.Querier(mint, maxt))
	}

	return q
}

// SeriesSet contains a set of series.
type SeriesSet interface {
	Next() bool
	Series() Series
	Err() error
}

func (q *querier) Select(ms ...Matcher) SeriesSet {
	// We gather the non-overlapping series from every shard and simply
	// return their union.
	r := &mergedSeriesSet{}

	for _, s := range q.shards {
		r.sets = append(r.sets, s.Select(ms...))
	}
	return r
}

func (q *querier) LabelValues(string) ([]string, error) {
	return nil, nil
}

func (q *querier) LabelValuesFor(string, Label) ([]string, error) {
	return nil, fmt.Errorf("not implemented")
}

func (q *querier) Close() error {
	return nil
}

// shardQuerier aggregates querying results from time blocks within
// a single shard.
type shardQuerier struct {
	blocks []Querier
}

// Querier returns a new querier over the data shard for the given
// time range.
func (s *SeriesShard) Querier(mint, maxt int64) Querier {
	blocks := s.blocksForRange(mint, maxt)

	sq := &shardQuerier{
		blocks: make([]Querier, 0, len(blocks)),
	}
	for _, b := range blocks {
		sq.blocks = append(sq.blocks, b.Querier(mint, maxt))
	}

	return sq
}

type mergedSeriesSet struct {
	sets []SeriesSet

	cur int
	err error
}

func (s *mergedSeriesSet) Series() Series { return s.sets[s.cur].Series() }
func (s *mergedSeriesSet) Err() error     { return s.sets[s.cur].Err() }

func (s *mergedSeriesSet) Next() bool {
	// TODO(fabxc): We just emit the sets one after one. They are each
	// lexicographically sorted. Should we emit their union sorted too?
	if s.sets[s.cur].Next() {
		return true
	}
	s.cur++

	if s.cur == len(s.sets) {
		return false
	}
	return s.Next()
}

type shardSeriesSet struct {
	a, b SeriesSet

	cur    Series
	as, bs Series // peek ahead of each set
}

func newShardSeriesSet(a, b SeriesSet) *shardSeriesSet {
	s := &shardSeriesSet{a: a, b: b}
	// Initialize first elements of both sets as Next() needs
	// one element look-ahead.
	s.advanceA()
	s.advanceB()

	return s
}

// compareLabels compares the two label sets.
// The result will be 0 if a==b, <0 if a < b, and >0 if a > b.
func compareLabels(a, b Labels) int {
	l := len(a)
	if len(b) < l {
		l = len(b)
	}

	for i := 0; i < l; i++ {
		if d := strings.Compare(a[i].Name, b[i].Name); d != 0 {
			return d
		}
	}
	// If all labels so far were in common, the set with fewer labels comes first.
	return len(b) - len(a)
}

func (s *shardSeriesSet) Series() Series {
	return s.cur
}

func (s *shardSeriesSet) Err() error {
	if s.a.Err() != nil {
		return s.a.Err()
	}
	return s.b.Err()
}

func (s *shardSeriesSet) compare() int {
	if s.as == nil {
		return 1
	}
	if s.bs == nil {
		return -1
	}
	return compareLabels(s.as.Labels(), s.bs.Labels())
}

func (s *shardSeriesSet) advanceA() {
	if s.a.Next() {
		s.as = s.a.Series()
	} else {
		s.as = nil
	}
}

func (s *shardSeriesSet) advanceB() {
	if s.b.Next() {
		s.bs = s.b.Series()
	} else {
		s.bs = nil
	}
}

func (s *shardSeriesSet) Next() bool {
	if s.as == nil && s.bs == nil {
		return false
	}

	d := s.compare()
	// Both sets contain the current series. Chain them into a single one.
	if d > 0 {
		s.cur = s.bs
		s.advanceB()

	} else if d < 0 {
		s.cur = s.as
		s.advanceA()

	} else {
		s.cur = &chainedSeries{series: []Series{s.as, s.bs}}
		s.advanceA()
		s.advanceB()
	}
	return true
}

func (q *shardQuerier) Select(ms ...Matcher) SeriesSet {
	// Sets from different blocks have no time overlap. The reference numbers
	// they emit point to series sorted in lexicographic order.
	// We can fully connect partial series by simply comparing with the previous
	// label set.
	if len(q.blocks) == 0 {
		return nil
	}
	r := q.blocks[0].Select(ms...)

	for _, s := range q.blocks[1:] {
		r = &shardSeriesSet{a: r, b: s.Select(ms...)}
	}

	return r
}

func (q *shardQuerier) LabelValues(string) ([]string, error) {
	return nil, nil
}

func (q *shardQuerier) LabelValuesFor(string, Label) ([]string, error) {
	return nil, fmt.Errorf("not implemented")
}

func (q *shardQuerier) Close() error {
	return nil
}

// blockQuerier provides querying access to a single block database.
type blockQuerier struct {
	index  IndexReader
	series SeriesReader

	mint, maxt int64
}

func newBlockQuerier(ix IndexReader, s SeriesReader, mint, maxt int64) *blockQuerier {
	return &blockQuerier{
		mint:   mint,
		maxt:   maxt,
		index:  ix,
		series: s,
	}
}

func (q *blockQuerier) Select(ms ...Matcher) SeriesSet {
	var its []Iterator
	for _, m := range ms {
		its = append(its, q.selectSingle(m))
	}

	// TODO(fabxc): pass down time range so the series iterator
	// can be instantiated with it?
	return &blockSeriesSet{
		index: q.index,
		it:    Intersect(its...),
	}
}

func (q *blockQuerier) selectSingle(m Matcher) Iterator {
	tpls, err := q.index.LabelValues(m.Name())
	if err != nil {
		return errIterator{err: err}
	}
	// TODO(fabxc): use interface upgrading to provide fast solution
	// for equality and prefix matches. Tuples are lexicographically sorted.
	var res []string

	for i := 0; i < tpls.Len(); i++ {
		vals, err := tpls.At(i)
		if err != nil {
			return errIterator{err: err}
		}
		if m.Match(vals[0]) {
			res = append(res, vals[0])
		}
	}

	var rit Iterator

	for _, v := range res {
		it, err := q.index.Postings(m.Name(), v)
		if err != nil {
			return errIterator{err: err}
		}
		rit = Intersect(rit, it)
	}

	return rit
}

func (q *blockQuerier) LabelValues(name string) ([]string, error) {
	tpls, err := q.index.LabelValues(name)
	if err != nil {
		return nil, err
	}
	res := make([]string, 0, tpls.Len())

	for i := 0; i < tpls.Len(); i++ {
		vals, err := tpls.At(i)
		if err != nil {
			return nil, err
		}
		res = append(res, vals[0])
	}
	return nil, nil
}

func (q *blockQuerier) LabelValuesFor(string, Label) ([]string, error) {
	return nil, fmt.Errorf("not implemented")
}

func (q *blockQuerier) Close() error {
	return nil
}

// blockSeriesSet is a set of series from an inverted index query.
type blockSeriesSet struct {
	index IndexReader
	it    Iterator

	err error
	cur Series
}

func (s *blockSeriesSet) Next() bool {
	// Get next reference from postings iterator.
	if !s.it.Next() {
		s.err = s.it.Err()
		return false
	}

	// Resolve reference to series.
	series, err := s.index.Series(s.it.Value())
	if err != nil {
		s.err = err
		return false
	}

	s.cur = series
	return true
}

func (s *blockSeriesSet) Series() Series { return s.cur }
func (s *blockSeriesSet) Err() error     { return s.err }

// SeriesIterator iterates over the data of a time series.
type SeriesIterator interface {
	// Seek advances the iterator forward to the given timestamp.
	// If there's no value exactly at ts, it advances to the last value
	// before tt.
	Seek(t int64) bool
	// Values returns the current timestamp/value pair.
	Values() (t int64, v float64)
	// Next advances the iterator by one.
	Next() bool
	// Err returns the current error.
	Err() error
}

type chainedSeries struct {
	series []Series
}

func (s *chainedSeries) Labels() Labels {
	return s.series[0].Labels()
}

func (s *chainedSeries) Iterator() SeriesIterator {
	it := &chainedSeriesIterator{
		series: make([]SeriesIterator, 0, len(s.series)),
	}
	for _, series := range s.series {
		it.series = append(it.series, series.Iterator())
	}
	return it
}

// chainedSeriesIterator implements a series iterater over a list
// of time-sorted, non-overlapping chunks.
type chainedSeriesIterator struct {
	series []SeriesIterator
}

func (it *chainedSeriesIterator) Seek(t int64) bool {
	return false
}

func (it *chainedSeriesIterator) Values() (t int64, v float64) {
	return 0, 0
}

func (it *chainedSeriesIterator) Next() bool {
	return false
}

func (it *chainedSeriesIterator) Err() error {
	return nil
}

// chunkSeriesIterator implements a series iterator on top
// of a list of time-sorted, non-overlapping chunks.
type chunkSeriesIterator struct {
	// minTimes []int64
	chunks []chunks.Chunk

	i   int
	cur chunks.Iterator
	err error
}

func newChunkSeriesIterator(cs []chunks.Chunk) *chunkSeriesIterator {
	return &chunkSeriesIterator{
		chunks: cs,
		i:      0,
		cur:    cs[0].Iterator(),
	}
}

func (it *chunkSeriesIterator) Seek(t int64) (ok bool) {
	// TODO(fabxc): skip to relevant chunk.
	for it.Next() {
		if ts, _ := it.Values(); ts >= t {
			return true
		}
	}
	return false
}

func (it *chunkSeriesIterator) Values() (t int64, v float64) {
	return it.cur.Values()
}

func (it *chunkSeriesIterator) Next() bool {
	if it.cur.Next() {
		return true
	}
	if err := it.cur.Err(); err != nil {
		return false
	}
	if it.i == len(it.chunks)-1 {
		return false
	}

	it.i++
	it.cur = it.chunks[it.i].Iterator()

	return it.Next()
}

func (it *chunkSeriesIterator) Err() error {
	return it.cur.Err()
}

type bufferedSeriesIterator struct {
	// TODO(fabxc): time-based look back buffer for time-aggregating
	// queries such as rate. It should allow us to re-use an iterator
	// within a range query while calculating time-aggregates at any point.
	//
	// It also allows looking up/seeking at-or-before without modifying
	// the simpler interface.
	//
	// Consider making this the main external interface.
	SeriesIterator

	buf []sample // lookback buffer
	i   int      // current head
}

type sample struct {
	t int64
	v float64
}

func (b *bufferedSeriesIterator) PeekBack(i int) (t int64, v float64, ok bool) {
	return 0, 0, false
}