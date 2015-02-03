// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package retrieval

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	clientmodel "github.com/prometheus/client_golang/model"

	"github.com/prometheus/prometheus/utility"
)

type collectResultIngester struct {
	result clientmodel.Samples
}

func (i *collectResultIngester) Ingest(s clientmodel.Samples) error {
	i.result = s
	return nil
}

func TestTargetScrapeUpdatesState(t *testing.T) {
	testTarget := target{
		state:      Unknown,
		url:        "bad schema",
		httpClient: utility.NewDeadlineClient(0),
	}
	testTarget.scrape(nopIngester{})
	if testTarget.state != Unreachable {
		t.Errorf("Expected target state %v, actual: %v", Unreachable, testTarget.state)
	}
}

func TestTargetRecordScrapeHealth(t *testing.T) {
	testTarget := target{
		url:        "http://example.url",
		baseLabels: clientmodel.LabelSet{clientmodel.JobLabel: "testjob"},
		httpClient: utility.NewDeadlineClient(0),
	}

	now := clientmodel.Now()
	ingester := &collectResultIngester{}
	testTarget.recordScrapeHealth(ingester, now, true, 2*time.Second)

	result := ingester.result

	if len(result) != 2 {
		t.Fatalf("Expected two samples, got %d", len(result))
	}

	actual := result[0]
	expected := &clientmodel.Sample{
		Metric: clientmodel.Metric{
			clientmodel.MetricNameLabel: scrapeHealthMetricName,
			InstanceLabel:               "http://example.url",
			clientmodel.JobLabel:        "testjob",
		},
		Timestamp: now,
		Value:     1,
	}

	if !actual.Equal(expected) {
		t.Fatalf("Expected and actual samples not equal. Expected: %v, actual: %v", expected, actual)
	}

	actual = result[1]
	expected = &clientmodel.Sample{
		Metric: clientmodel.Metric{
			clientmodel.MetricNameLabel: scrapeDurationMetricName,
			InstanceLabel:               "http://example.url",
			clientmodel.JobLabel:        "testjob",
		},
		Timestamp: now,
		Value:     2.0,
	}

	if !actual.Equal(expected) {
		t.Fatalf("Expected and actual samples not equal. Expected: %v, actual: %v", expected, actual)
	}
}

func TestTargetScrapeTimeout(t *testing.T) {
	signal := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-signal
		w.Header().Set("Content-Type", `application/json; schema="prometheus/telemetry"; version=0.0.2`)
		w.Write([]byte(`[]`))
	}))

	defer server.Close()

	testTarget := NewTarget(server.URL, 10*time.Millisecond, clientmodel.LabelSet{}, nil)
	ingester := nopIngester{}

	// scrape once without timeout
	signal <- true
	if err := testTarget.(*target).scrape(ingester); err != nil {
		t.Fatal(err)
	}

	// let the deadline lapse
	time.Sleep(15 * time.Millisecond)

	// now scrape again
	signal <- true
	if err := testTarget.(*target).scrape(ingester); err != nil {
		t.Fatal(err)
	}

	// now timeout
	if err := testTarget.(*target).scrape(ingester); err == nil {
		t.Fatal("expected scrape to timeout")
	} else {
		signal <- true // let handler continue
	}

	// now scrape again without timeout
	signal <- true
	if err := testTarget.(*target).scrape(ingester); err != nil {
		t.Fatal(err)
	}
}

func TestTargetScrape404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	defer server.Close()

	testTarget := NewTarget(server.URL, 10*time.Millisecond, clientmodel.LabelSet{}, nil)
	ingester := nopIngester{}

	want := errors.New("server returned HTTP status 404 Not Found")
	got := testTarget.(*target).scrape(ingester)
	if got == nil || want.Error() != got.Error() {
		t.Fatalf("want err %q, got %q", want, got)
	}
}

func TestTargetRunScraperScrapes(t *testing.T) {
	testTarget := target{
		state:           Unknown,
		url:             "bad schema",
		httpClient:      utility.NewDeadlineClient(0),
		scraperStopping: make(chan struct{}),
		scraperStopped:  make(chan struct{}),
	}
	go testTarget.RunScraper(nopIngester{}, time.Duration(time.Millisecond))

	// Enough time for a scrape to happen.
	time.Sleep(2 * time.Millisecond)
	if testTarget.lastScrape.IsZero() {
		t.Errorf("Scrape hasn't occured.")
	}

	testTarget.StopScraper()
	// Wait for it to take effect.
	time.Sleep(2 * time.Millisecond)
	last := testTarget.lastScrape
	// Enough time for a scrape to happen.
	time.Sleep(2 * time.Millisecond)
	if testTarget.lastScrape != last {
		t.Errorf("Scrape occured after it was stopped.")
	}
}
