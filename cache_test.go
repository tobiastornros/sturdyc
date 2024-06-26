package sturdyc_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/creativecreature/sturdyc"
)

type distributionTestCase struct {
	name                string
	capacity            int
	numShards           int
	tolerancePercentage int
	keyLength           int
}

func TestShardDistribution(t *testing.T) {
	t.Parallel()

	testCases := []distributionTestCase{
		{
			name:                "1_000_000 capacity, 100 shards, 8% tolerance, 16 key length",
			capacity:            1_000_000,
			numShards:           100,
			tolerancePercentage: 12,
			keyLength:           16,
		},
		{
			name:                "1000 capacity, 2 shards, 8% tolerance, 14 key length",
			capacity:            1000,
			numShards:           2,
			tolerancePercentage: 12,
			keyLength:           14,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recorder := newTestMetricsRecorder(tc.numShards)
			client := sturdyc.New(tc.capacity, tc.numShards, time.Hour, 5, sturdyc.WithMetrics(recorder))
			for i := 0; i < tc.capacity; i++ {
				key := randKey(tc.keyLength)
				sturdyc.Set(client, key, "value")
			}
			recorder.validateShardDistribution(t, tc.tolerancePercentage)
		})
	}
}

func TestTimeBasedEviction(t *testing.T) {
	t.Parallel()
	capacity := 10_000
	numShards := 100
	ttl := time.Hour
	evictionPercentage := 5
	evictionInterval := time.Second
	clock := sturdyc.NewTestClock(time.Now())
	metricRecorder := newTestMetricsRecorder(numShards)
	client := sturdyc.New(
		capacity,
		numShards,
		ttl,
		evictionPercentage,
		sturdyc.WithMetrics(metricRecorder),
		sturdyc.WithClock(clock),
		sturdyc.WithEvictionInterval(evictionInterval),
	)

	for i := 0; i < capacity; i++ {
		sturdyc.Set(client, randKey(12), "value")
	}

	// Expire all entries.
	clock.Add(ttl + 1)

	// Next, we'll loop through each shard while moving the clock by the evictionInterval. We'll
	// sleep for a brief duration to allow the goroutines that were waiting for the timer to run.
	for i := 0; i < numShards; i++ {
		clock.Add(time.Second + 1)
		time.Sleep(5 * time.Millisecond)
	}

	metricRecorder.Lock()
	defer metricRecorder.Unlock()
	if metricRecorder.evictedEntries != capacity {
		t.Errorf("expected %d evicted entries, got %d", capacity, metricRecorder.evictedEntries)
	}
}

type forcedEvictionTestCase struct {
	name               string
	capacity           int
	writes             int
	numShards          int
	evictionPercentage int
	minEvictions       int
	maxEvictions       int
}

func TestForcedEvictions(t *testing.T) {
	t.Parallel()

	testCases := []forcedEvictionTestCase{
		{
			name:               "1000 capacity, 100_000 writes, 100 shards, 5% forced evictions",
			capacity:           10_000,
			writes:             100_000,
			numShards:          100,
			evictionPercentage: 5,
			minEvictions:       20_000, // Perfect shard distribution.
			maxEvictions:       20_800, // Accounting for a 4% tolerance.
		},
		{
			name:               "100 capacity, 10_000 writes, 10 shards, 1% forced evictions",
			capacity:           100,
			writes:             10_000,
			numShards:          10,
			evictionPercentage: 1,
			minEvictions:       9999,
			maxEvictions:       10001,
		},
		{
			name:               "100 capacity, 1000 writes, 10 shards, 100% forced evictions",
			capacity:           100,
			writes:             1000,
			numShards:          10,
			evictionPercentage: 100,
			minEvictions:       100,
			maxEvictions:       120,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			recorder := newTestMetricsRecorder(tc.numShards)
			client := sturdyc.New(tc.capacity,
				tc.numShards,
				time.Hour,
				tc.evictionPercentage,
				sturdyc.WithMetrics(recorder),
			)

			// Start by filling the sturdyc.
			for i := 0; i < tc.capacity; i++ {
				key := randKey(12)
				sturdyc.Set(client, key, "value")
			}

			// Next, we'll write to the cache to force evictions.
			for i := 0; i < tc.writes; i++ {
				key := randKey(12)
				sturdyc.Set(client, key, "value")
			}

			if recorder.forcedEvictions < tc.minEvictions || recorder.forcedEvictions > tc.maxEvictions {
				t.Errorf(
					"expected forced evictions between %d and %d, got %d",
					tc.minEvictions, tc.maxEvictions, recorder.forcedEvictions,
				)
			}
		})
	}
}

func TestDisablingForcedEvictionMakesSetANoop(t *testing.T) {
	t.Parallel()

	capacity := 100
	numShards := 10
	ttl := time.Hour
	// Setting the eviction percentage to 0 should disable forced evictions.
	evictionpercentage := 0
	metricRecorder := newTestMetricsRecorder(numShards)
	c := sturdyc.New(
		capacity,
		numShards,
		ttl,
		evictionpercentage,
		sturdyc.WithMetrics(metricRecorder),
	)

	for i := 0; i < capacity*10; i++ {
		sturdyc.Set(c, randKey(12), "value")
	}

	metricRecorder.Lock()
	defer metricRecorder.Unlock()
	if metricRecorder.forcedEvictions > 0 {
		t.Errorf("expected no forced evictions, got %d", metricRecorder.forcedEvictions)
	}
}

func TestGetFetch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	capacity := 5
	numShards := 2
	ttl := time.Minute
	evictionPercentage := 10
	c := sturdyc.New(capacity, numShards, ttl, evictionPercentage)

	id := "1"
	fetchObserver := NewFetchObserver(1)
	fetchObserver.Response(id)

	// The first time we call Get, it should call the fetchFn to retrieve the value.
	firstValue, err := sturdyc.GetFetch(ctx, c, id, fetchObserver.Fetch)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if firstValue != "value1" {
		t.Errorf("expected value1, got %v", firstValue)
	}

	<-fetchObserver.FetchCompleted
	fetchObserver.AssertFetchCount(t, 1)

	// The second time we call Get, we expect to have it served from the sturdyc.
	secondValue, err := sturdyc.GetFetch(ctx, c, id, fetchObserver.Fetch)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if secondValue != "value1" {
		t.Errorf("expected value1, got %v", secondValue)
	}
	time.Sleep(time.Millisecond * 10)
	fetchObserver.AssertFetchCount(t, 1)
}

func TestGetFetchStampedeProtection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	capacity := 10
	numShards := 2
	ttl := time.Second * 2
	evictionPercentage := 10
	clock := sturdyc.NewTestClock(time.Now())
	minRefreshDelay := time.Millisecond * 500
	maxRefreshDelay := time.Millisecond * 1000
	refreshRetryInterval := time.Millisecond * 10

	// The cache is going to have a 2 second TTL, and the first refresh should happen within a second.
	client := sturdyc.New(capacity, numShards, ttl, evictionPercentage,
		sturdyc.WithStampedeProtection(minRefreshDelay, maxRefreshDelay, refreshRetryInterval, true),
		sturdyc.WithClock(clock),
	)

	id := "1"
	fetchObserver := NewFetchObserver(1)
	fetchObserver.Response(id)

	// We will start the test by trying to get key1, which wont exist in the sturdyc. Hence,
	// the fetch function is going to get called and we'll set the initial value to val1.
	sturdyc.GetFetch[string](ctx, client, id, fetchObserver.Fetch)

	<-fetchObserver.FetchCompleted
	fetchObserver.AssertFetchCount(t, 1)

	// Now, we're going to go past the refresh delay and try to refresh it from 1000 goroutines at once.
	numGoroutines := 1000
	clock.Add(maxRefreshDelay + 1)
	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			_, err := sturdyc.GetFetch(ctx, client, id, fetchObserver.Fetch)
			if err != nil {
				panic(err)
			}
		}()
	}
	wg.Wait()

	<-fetchObserver.FetchCompleted
	fetchObserver.AssertFetchCount(t, 2)
}

func TestGetFetchRefreshRetries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	capacity := 5
	numShards := 1
	ttl := time.Minute
	evictionPercentage := 10
	minRefreshDelay := time.Second
	maxRefreshDelay := time.Second * 2
	retryInterval := time.Millisecond * 10
	clock := sturdyc.NewTestClock(time.Now())

	c := sturdyc.New(capacity, numShards, ttl, evictionPercentage,
		sturdyc.WithStampedeProtection(minRefreshDelay, maxRefreshDelay, retryInterval, true),
		sturdyc.WithClock(clock),
	)

	id := "1"
	fetchObserver := NewFetchObserver(6)
	fetchObserver.Response(id)

	_, err := sturdyc.GetFetch(ctx, c, id, fetchObserver.Fetch)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	<-fetchObserver.FetchCompleted
	fetchObserver.AssertFetchCount(t, 1)
	fetchObserver.Clear()

	// Now, we'll move the clock passed the refresh delay which should make the
	// next call to GetFetchBatch result in a call to refresh the record.
	clock.Add(maxRefreshDelay + 1)
	fetchObserver.Err(errors.New("error"))
	_, err = sturdyc.GetFetch(ctx, c, id, fetchObserver.Fetch)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// Next, we'll assert that the retries grows exponentially. Even though we're
	// making 100 requests with 1 second between them, we only expect 6 calls to
	// go through.
	for i := 0; i < 100; i++ {
		clock.Add(retryInterval)
		sturdyc.GetFetch(ctx, c, id, fetchObserver.Fetch)
	}
	for i := 0; i < 6; i++ {
		<-fetchObserver.FetchCompleted
	}
	fetchObserver.AssertMaxFetchCount(t, 8)
}

func TestGetFetchMissingRecord(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	capacity := 5
	numShards := 1
	ttl := time.Minute
	evictionPercentage := 20
	minRefreshDelay := time.Second
	maxRefreshDelay := time.Second * 2
	retryInterval := time.Millisecond * 10
	clock := sturdyc.NewTestClock(time.Now())
	c := sturdyc.New(capacity, numShards, ttl, evictionPercentage,
		sturdyc.WithClock(clock),
		sturdyc.WithStampedeProtection(minRefreshDelay, maxRefreshDelay, retryInterval, true),
	)

	fetchObserver := NewFetchObserver(1)
	fetchObserver.Err(sturdyc.ErrStoreMissingRecord)
	_, err := sturdyc.GetFetch(ctx, c, "1", fetchObserver.Fetch)
	if !errors.Is(err, sturdyc.ErrStoreMissingRecord) {
		t.Fatalf("expected ErrStoreMissingRecord, got %v", err)
	}
	<-fetchObserver.FetchCompleted
	fetchObserver.AssertFetchCount(t, 1)
	fetchObserver.Clear()

	// Make the request again. It should trigger the refresh of the missing record to happen in the background.
	clock.Add(maxRefreshDelay * 1)
	fetchObserver.Response("1")
	_, err = sturdyc.GetFetch(ctx, c, "1", fetchObserver.Fetch)
	if !errors.Is(err, sturdyc.ErrMissingRecord) {
		t.Fatalf("expected ErrMissingRecordCooldown, got %v", err)
	}
	<-fetchObserver.FetchCompleted
	fetchObserver.AssertFetchCount(t, 2)

	// The next time we call the cache, the record should be there.
	val, err := sturdyc.GetFetch(ctx, c, "1", fetchObserver.Fetch)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if val != "value1" {
		t.Errorf("expected value to be value1, got %v", val)
	}
	fetchObserver.AssertFetchCount(t, 2)
}

func TestGetFetchBatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	c := sturdyc.New(5, 1, time.Minute, 30)
	fetchObserver := NewFetchObserver(1)

	firstBatchOfIDs := []string{"1", "2", "3"}
	fetchObserver.BatchResponse(firstBatchOfIDs)
	_, err := sturdyc.GetFetchBatch(ctx, c, firstBatchOfIDs, c.BatchKeyFn("item"), fetchObserver.FetchBatch)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	<-fetchObserver.FetchCompleted
	fetchObserver.AssertRequestedRecords(t, firstBatchOfIDs)
	fetchObserver.AssertFetchCount(t, 1)
	fetchObserver.Clear()

	// At this point, id 1, 2, and 3 should be in the sturdyc. Therefore, if we make a second
	// request where we'll request item 1, 2, 3, and 4, we'll only expect that item 4 is fetched.
	secondBatchOfIDs := []string{"1", "2", "3", "4"}
	fetchObserver.BatchResponse([]string{"4"})
	_, err = sturdyc.GetFetchBatch(ctx, c, secondBatchOfIDs, c.BatchKeyFn("item"), fetchObserver.FetchBatch)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	<-fetchObserver.FetchCompleted
	fetchObserver.AssertRequestedRecords(t, []string{"4"})
	fetchObserver.AssertFetchCount(t, 2)
	fetchObserver.Clear()

	// The last scenario we want to test is for partial responses. This time, we'll request ids 2, 4, and 6. The item with
	// id 6 isn't in our cache yet, so the fetch function should get invoked. However, we'll make the fetch function error.
	// This should give us a ErrOnlyCachedRecords error, along with the records we could retrieve from the sturdyc.
	thirdBatchOfIDs := []string{"2", "4", "6"}
	fetchObserver.Err(errors.New("error"))
	records, err := sturdyc.GetFetchBatch(ctx, c, thirdBatchOfIDs, c.BatchKeyFn("item"), fetchObserver.FetchBatch)
	<-fetchObserver.FetchCompleted
	fetchObserver.AssertRequestedRecords(t, []string{"6"})
	if !errors.Is(err, sturdyc.ErrOnlyCachedRecords) {
		t.Errorf("expected ErrPartialBatchResponse, got %v", err)
	}
	if len(records) != 2 {
		t.Errorf("expected to get the two records we had cached, got %v", len(records))
	}
}

func TestBatchGetFetchNilMapMissingRecords(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	capacity := 5
	numShards := 1
	ttl := time.Minute
	evictionPercentage := 50
	minRefreshDelay := time.Minute
	maxRefreshDelay := time.Minute * 2
	retryInterval := time.Second
	clock := sturdyc.NewTestClock(time.Now())
	c := sturdyc.New(capacity, numShards, ttl, evictionPercentage,
		sturdyc.WithStampedeProtection(minRefreshDelay, maxRefreshDelay, retryInterval, true),
		sturdyc.WithClock(clock),
	)

	fetchObserver := NewFetchObserver(1)
	ids := []string{"1", "2", "3", "4"}
	records, err := sturdyc.GetFetchBatch(ctx, c, ids, c.BatchKeyFn("item"), fetchObserver.FetchBatch)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected no records, got %v", records)
	}
	<-fetchObserver.FetchCompleted
	fetchObserver.AssertRequestedRecords(t, ids)
	fetchObserver.AssertFetchCount(t, 1)

	// The request didn't return any records, and we have configured the cache to
	// store these ids as cache misses. Hence, performing the request again,
	// should not result in another call before the refresh delay has passed.
	clock.Add(minRefreshDelay - 1)
	records, err = sturdyc.GetFetchBatch(ctx, c, ids, c.BatchKeyFn("item"), fetchObserver.FetchBatch)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected no records, got %v", records)
	}
	time.Sleep(time.Millisecond * 10)
	fetchObserver.AssertRequestedRecords(t, ids)
	fetchObserver.AssertFetchCount(t, 1)
}

func TestGetFetchBatchRetries(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	capacity := 5
	numShards := 1
	ttl := time.Hour * 24
	evictionPercentage := 10
	minRefreshDelay := time.Hour
	maxRefreshDelay := time.Hour * 2
	retryInterval := time.Second
	clock := sturdyc.NewTestClock(time.Now())
	c := sturdyc.New(capacity, numShards, ttl, evictionPercentage,
		sturdyc.WithStampedeProtection(minRefreshDelay, maxRefreshDelay, retryInterval, true),
		sturdyc.WithClock(clock),
	)
	fetchObserver := NewFetchObserver(6)

	ids := []string{"1", "2", "3"}
	fetchObserver.BatchResponse(ids)

	// Assert that all records were requested, and that we retrieved each one of them.
	_, err := sturdyc.GetFetchBatch(ctx, c, ids, c.BatchKeyFn("item"), fetchObserver.FetchBatch)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	<-fetchObserver.FetchCompleted
	fetchObserver.AssertRequestedRecords(t, ids)
	fetchObserver.AssertFetchCount(t, 1)
	fetchObserver.Clear()

	// Now, we'll move the clock passed the refresh delay which should make the
	// next call to GetFetchBatch result in a call to refresh the record.
	clock.Add(maxRefreshDelay + 1)
	fetchObserver.Err(errors.New("error"))
	_, err = sturdyc.GetFetchBatch(ctx, c, ids, c.BatchKeyFn("item"), fetchObserver.FetchBatch)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	<-fetchObserver.FetchCompleted
	fetchObserver.AssertRequestedRecords(t, ids)
	fetchObserver.AssertFetchCount(t, 2)

	// Next, we'll assert that the retries grows exponentially. Even though we're
	// making 100 requests with 1 second between them, we only expect 6 calls to
	// go through.
	for i := 0; i < 100; i++ {
		clock.Add(retryInterval)
		sturdyc.GetFetchBatch(ctx, c, ids, c.BatchKeyFn("item"), fetchObserver.FetchBatch)
	}
	for i := 0; i < 6; i++ {
		<-fetchObserver.FetchCompleted
	}
	fetchObserver.AssertFetchCount(t, 8)
}

func TestBatchGetFetchOnlyCachedRecordsErr(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	capacity := 5
	numShards := 1
	ttl := time.Minute
	evictionPercentage := 10
	clock := sturdyc.NewTestClock(time.Now())
	c := sturdyc.New(capacity, numShards, ttl, evictionPercentage, sturdyc.WithClock(clock))
	fetchObserver := NewFetchObserver(1)

	// We'll start by fetching a couple of ids without any errors to fill the sturdyc.
	ids := []string{"1", "2", "3", "4"}
	fetchObserver.BatchResponse(ids)
	_, firstBatchErr := sturdyc.GetFetchBatch(ctx, c, ids, c.BatchKeyFn("item"), fetchObserver.FetchBatch)
	if firstBatchErr != nil {
		t.Errorf("expected no error, got %v", firstBatchErr)
	}

	<-fetchObserver.FetchCompleted
	fetchObserver.AssertRequestedRecords(t, ids)
	fetchObserver.AssertFetchCount(t, 1)
	fetchObserver.Clear()

	// Now, we'll append "5" to our slice of ids. After that we'll try to fetch the
	// records again. This time with a BatchFn that returns an error. That should give
	// us a ErrOnlyCachedRecords error along with the records we had in the sturdyc.
	// This allows the caller to decide if they want to proceed or not.
	ids = append(ids, "5")
	fetchObserver.Err(errors.New("error"))
	records, secondBatchErr := sturdyc.GetFetchBatch(ctx, c, ids, c.BatchKeyFn("item"), fetchObserver.FetchBatch)

	if !errors.Is(secondBatchErr, sturdyc.ErrOnlyCachedRecords) {
		t.Errorf("expected ErrPartialBatchResponse, got %v", secondBatchErr)
	}
	// We should have a record for every id except the last one.
	if len(records) != len(ids)-1 {
		t.Errorf("expected to get %v records, got %v", len(ids)-1, len(records))
	}

	<-fetchObserver.FetchCompleted
	fetchObserver.AssertRequestedRecords(t, []string{"5"})
	fetchObserver.AssertFetchCount(t, 2)
}

func TestGetFetchBatchStampedeProtection(t *testing.T) {
	t.Parallel()

	// We're going to fetch the same list of ids in 1000 goroutines.
	numGoroutines := 1000
	ctx := context.Background()
	capacity := 10
	shards := 2
	ttl := time.Second * 2
	evictionPercentage := 5
	clock := sturdyc.NewTestClock(time.Now())
	minRefreshDelay := time.Millisecond * 500
	maxRefreshDelay := time.Millisecond * 1000
	refreshRetryInterval := time.Millisecond * 10
	client := sturdyc.New(capacity, shards, ttl, evictionPercentage,
		sturdyc.WithStampedeProtection(minRefreshDelay, maxRefreshDelay, refreshRetryInterval, true),
		sturdyc.WithClock(clock),
	)

	ids := []string{"1", "2", "3"}
	fetchObserver := NewFetchObserver(1000)
	fetchObserver.BatchResponse(ids)

	_, err := sturdyc.GetFetchBatch(ctx, client, ids, client.BatchKeyFn("item"), fetchObserver.FetchBatch)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	<-fetchObserver.FetchCompleted
	fetchObserver.AssertRequestedRecords(t, ids)
	fetchObserver.AssertFetchCount(t, 1)
	fetchObserver.Clear()

	// Set the clock to be just before the min cache refresh threshold.
	// This should not be enough to make the cache call our fetchFn.
	clock.Add(minRefreshDelay - 1)
	_, err = sturdyc.GetFetchBatch(ctx, client, ids, client.BatchKeyFn("item"), fetchObserver.FetchBatch)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// We don't expect fetchObserver.Fetch to have been called. Therfore, we'll
	// sleep for a brief duration and asser that the fetch count is still 1.
	time.Sleep(time.Millisecond * 10)
	fetchObserver.AssertFetchCount(t, 1)

	// Now, let's go past the threshold. This should make the next GetFetchBatch
	// call schedule a refresh in the background, and with that we're going to
	// test that the stampede protection works as intended. Invoking it from 1000
	// goroutines at the same time should not make us schedule multiple refreshes.
	clock.Add((maxRefreshDelay - minRefreshDelay) + 1)
	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			_, goroutineErr := sturdyc.GetFetchBatch(ctx, client, ids, client.BatchKeyFn("item"), fetchObserver.FetchBatch)
			if goroutineErr != nil {
				panic(goroutineErr)
			}
		}()
	}
	wg.Wait()

	// Even though we called GetFetch 1000 times, it should only result in a
	// maximum of 3 outgoing requests. Most likely, it should just be one
	// additional request but without having a top level lock in GetFetchBatch we
	// can't guarantee that the first goroutine moves the refreshAt of all 3 ids.
	// The first goroutine might get a lock for the first index, and then get paused.
	// During that time a second goroutine could have refreshed id 2 and 3.
	<-fetchObserver.FetchCompleted
	fetchObserver.AssertMaxFetchCount(t, 4)
}
