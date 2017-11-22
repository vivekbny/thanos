package query

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"testing"

	"github.com/prometheus/tsdb/chunks"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/improbable-eng/thanos/pkg/store/storepb"
	"github.com/improbable-eng/thanos/pkg/testutil"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/storage"
	"google.golang.org/grpc"
)

func TestQuerier_LabelValues(t *testing.T) {
	a := &testStoreClient{
		values: map[string][]string{
			"test": []string{"a", "b", "c", "d"},
		},
	}
	b := &testStoreClient{
		values: map[string][]string{
			// The contract is that label values are sorted but we should be resilient
			// to misbehaving clients.
			"test": []string{"a", "out-of-order", "d", "x", "y"},
		},
	}
	c := &testStoreClient{
		values: map[string][]string{
			"test": []string{"e"},
		},
	}
	expected := []string{"a", "b", "c", "d", "e", "out-of-order", "x", "y"}

	q := newQuerier(context.Background(), nil, []StoreInfo{
		testStoreInfo{client: a},
		testStoreInfo{client: b},
		testStoreInfo{client: c},
	}, 0, 10000, "")
	defer q.Close()

	vals, err := q.LabelValues("test")
	testutil.Ok(t, err)
	testutil.Equals(t, expected, vals)
}

// TestQuerier_Series catches common edge cases encountered when querying multiple store nodes.
// It is not a subtitute for testing fanin/merge procedures in depth.
func TestQuerier_Series(t *testing.T) {
	a := &testStoreClient{
		series: []storepb.Series{
			testStoreSeries(t, labels.FromStrings("a", "a"), []sample{{0, 0}, {2, 1}, {3, 2}}),
			testStoreSeries(t, labels.FromStrings("a", "b"), []sample{{2, 2}, {3, 3}, {4, 4}}),
		},
	}
	b := &testStoreClient{
		series: []storepb.Series{
			testStoreSeries(t, labels.FromStrings("a", "b"), []sample{{1, 1}, {2, 2}, {3, 3}}),
		},
	}
	c := &testStoreClient{
		series: []storepb.Series{
			testStoreSeries(t, labels.FromStrings("a", "c"), []sample{{100, 1}, {300, 3}, {400, 4}}),
		},
	}
	// Querier clamps the range to [1,300], which should drop some samples of the result above.
	// The store API allows endpoints to send more data then initially requested.
	q := newQuerier(context.Background(), nil, []StoreInfo{
		testStoreInfo{client: a},
		testStoreInfo{client: b},
		testStoreInfo{client: c},
	}, 1, 300, "")
	defer q.Close()

	res := q.Select()

	expected := []struct {
		lset    labels.Labels
		samples []sample
	}{
		{
			lset:    labels.FromStrings("a", "a"),
			samples: []sample{{2, 1}, {3, 2}},
		},
		{
			lset:    labels.FromStrings("a", "b"),
			samples: []sample{{1, 1}, {2, 2}, {3, 3}, {4, 4}},
		},
		{
			lset:    labels.FromStrings("a", "c"),
			samples: []sample{{100, 1}, {300, 3}},
		},
	}

	i := 0
	for res.Next() {
		testutil.Equals(t, expected[i].lset, res.At().Labels())

		samples := expandSeries(t, res.At().Iterator())
		testutil.Equals(t, expected[i].samples, samples)

		i++
	}
	testutil.Ok(t, res.Err())
}

func TestStoreSelectSingle(t *testing.T) {
	c := &testStoreClient{
		series: []storepb.Series{
			{Labels: []storepb.Label{
				{"a", "1"},
				{"b", "replica-1"},
				{"c", "3"},
			}},
			{Labels: []storepb.Label{
				{"a", "1"},
				{"b", "replica-1"},
				{"c", "3"},
				{"d", "4"},
			}},
			{Labels: []storepb.Label{
				{"a", "1"},
				{"b", "replica-1"},
				{"c", "4"},
			}},
			{Labels: []storepb.Label{
				{"a", "1"},
				{"b", "replica-2"},
				{"c", "3"},
			}},
		},
	}
	// Just verify we assembled the input data according to the store API contract.
	ok := sort.SliceIsSorted(c.series, func(i, j int) bool {
		return storepb.CompareLabels(c.series[i].Labels, c.series[j].Labels) < 0
	})
	testutil.Assert(t, ok, "input data unoreded")

	q := newQuerier(context.Background(), nil, nil, 0, 0, "b")

	res, err := q.selectSingle(c)
	testutil.Ok(t, err)

	exp := [][]storepb.Label{
		{
			{"a", "1"},
			{"c", "3"},
			{"b", "replica-1"},
		},
		{
			{"a", "1"},
			{"c", "3"},
			{"b", "replica-2"},
		},
		{
			{"a", "1"},
			{"c", "3"},
			{"d", "4"},
			{"b", "replica-1"},
		},
		{
			{"a", "1"},
			{"c", "4"},
			{"b", "replica-1"},
		},
	}
	var got [][]storepb.Label

	for res.Next() {
		lset, _ := res.At()
		got = append(got, lset)
	}
	testutil.Equals(t, exp, got)
}

func TestStoreMatches(t *testing.T) {
	mustMatcher := func(mt labels.MatchType, n, v string) *labels.Matcher {
		m, err := labels.NewMatcher(mt, n, v)
		testutil.Ok(t, err)
		return m
	}
	cases := []struct {
		s  StoreInfo
		ms []*labels.Matcher
		ok bool
	}{
		{
			s: testStoreInfo{labels: []storepb.Label{{"a", "b"}}},
			ms: []*labels.Matcher{
				mustMatcher(labels.MatchEqual, "b", "1"),
			},
			ok: true,
		},
		{
			s: testStoreInfo{labels: []storepb.Label{{"a", "b"}}},
			ms: []*labels.Matcher{
				mustMatcher(labels.MatchEqual, "a", "b"),
			},
			ok: true,
		},
		{
			s: testStoreInfo{labels: []storepb.Label{{"a", "b"}}},
			ms: []*labels.Matcher{
				mustMatcher(labels.MatchEqual, "a", "c"),
			},
			ok: false,
		},
		{
			s: testStoreInfo{labels: []storepb.Label{{"a", "b"}}},
			ms: []*labels.Matcher{
				mustMatcher(labels.MatchRegexp, "a", "b|c"),
			},
			ok: true,
		},
		{
			s: testStoreInfo{labels: []storepb.Label{{"a", "b"}}},
			ms: []*labels.Matcher{
				mustMatcher(labels.MatchNotEqual, "a", ""),
			},
			ok: true,
		},
	}

	for i, c := range cases {
		ok := storeMatches(c.s, c.ms...)
		testutil.Assert(t, c.ok == ok, "test case %d failed", i)
	}
}

type testStoreInfo struct {
	labels []storepb.Label
	client storepb.StoreClient
}

func (s testStoreInfo) Labels() []storepb.Label {
	return s.labels
}

func (s testStoreInfo) Client() storepb.StoreClient {
	return s.client
}

func expandSeries(t testing.TB, it storage.SeriesIterator) (res []sample) {
	for it.Next() {
		t, v := it.At()
		res = append(res, sample{t, v})
	}
	testutil.Ok(t, it.Err())
	return res
}

func testStoreSeries(t testing.TB, lset labels.Labels, smpls []sample) (s storepb.Series) {
	for _, l := range lset {
		s.Labels = append(s.Labels, storepb.Label{Name: l.Name, Value: l.Value})
	}
	c := chunks.NewXORChunk()
	a, err := c.Appender()
	testutil.Ok(t, err)

	for _, smpl := range smpls {
		a.Append(smpl.t, smpl.v)
	}
	s.Chunks = append(s.Chunks, storepb.Chunk{
		Type:    storepb.Chunk_XOR,
		MinTime: smpls[0].t,
		MaxTime: smpls[len(smpls)-1].t,
		Data:    c.Bytes(),
	})
	return s
}

type testStoreClient struct {
	values map[string][]string
	series []storepb.Series
}

func (s *testStoreClient) Info(ctx context.Context, req *storepb.InfoRequest, _ ...grpc.CallOption) (*storepb.InfoResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (s *testStoreClient) Series(ctx context.Context, req *storepb.SeriesRequest, _ ...grpc.CallOption) (storepb.Store_SeriesClient, error) {
	return &testStoreSeriesClient{ctx: ctx, series: s.series}, nil
}

func (s *testStoreClient) LabelNames(ctx context.Context, req *storepb.LabelNamesRequest, _ ...grpc.CallOption) (*storepb.LabelNamesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (s *testStoreClient) LabelValues(ctx context.Context, req *storepb.LabelValuesRequest, _ ...grpc.CallOption) (*storepb.LabelValuesResponse, error) {
	return &storepb.LabelValuesResponse{Values: s.values[req.Label]}, nil
}

type testStoreSeriesClient struct {
	// This field just exist to pseudo-implement the unused methods of the interface.
	storepb.Store_SeriesClient
	ctx    context.Context
	series []storepb.Series
	i      int
}

func (c *testStoreSeriesClient) Recv() (*storepb.SeriesResponse, error) {
	if c.i >= len(c.series) {
		return nil, io.EOF
	}
	s := c.series[c.i]
	c.i++
	return &storepb.SeriesResponse{Series: s}, nil
}

func (c *testStoreSeriesClient) Context() context.Context {
	return c.ctx
}

func TestDedupSeriesSet(t *testing.T) {
	input := [][]storepb.Label{
		{
			{"a", "1"},
			{"c", "3"},
			{"replica", "replica-1"},
		}, {
			{"a", "1"},
			{"c", "3"},
			{"replica", "replica-2"},
		}, {
			{"a", "1"},
			{"c", "3"},
			{"replica", "replica-3"},
		}, {
			{"a", "1"},
			{"c", "3"},
			{"d", "4"},
		}, {
			{"a", "1"},
			{"c", "4"},
			{"replica", "replica-1"},
		}, {
			{"a", "2"},
			{"c", "3"},
			{"replica", "replica-3"},
		}, {
			{"a", "2"},
			{"c", "3"},
			{"replica", "replica-3"},
		},
	}
	var series []storepb.Series
	for _, lset := range input {
		series = append(series, storepb.Series{Labels: lset})
	}
	set := promSeriesSet{
		mint: math.MinInt64,
		maxt: math.MaxInt64,
		set:  newStoreSeriesSet(series),
	}
	dedupSet := newDedupSeriesSet(set, "replica")

	for dedupSet.Next() {
		fmt.Println(dedupSet.At())
	}
	testutil.Ok(t, dedupSet.Err())
}
