// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package systemtest_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elastic/apm-server/systemtest"
	"github.com/elastic/apm-server/systemtest/apmservertest"
	"github.com/elastic/apm-server/systemtest/estest"
)

func TestRUMXForwardedFor(t *testing.T) {
	systemtest.CleanupElasticsearch(t)
	srv := apmservertest.NewUnstartedServerTB(t)
	srv.Config.RUM = &apmservertest.RUMConfig{Enabled: true}
	err := srv.Start()
	require.NoError(t, err)

	serverURL, err := url.Parse(srv.URL)
	require.NoError(t, err)
	serverURL.Path = "/intake/v2/rum/events"

	// Send one transaction and one set of breakdown metrics.
	// They should both have geoIP enrichment applied.
	const body = `{"metadata":{"service":{"name":"rum-js-test","agent":{"name":"rum-js","version":"5.5.0"}}}}
{"transaction":{"trace_id":"611f4fa950f04631aaaaaaaaaaaaaaaa","id":"611f4fa950f04631","type":"page-load","duration":643,"span_count":{"started":0}}}
{"metricset":{"samples":{"transaction.breakdown.count":{"value":12},"transaction.duration.sum.us":{"value":12},"transaction.duration.count":{"value":2},"transaction.self_time.sum.us":{"value":10},"transaction.self_time.count":{"value":2},"span.self_time.count":{"value":1},"span.self_time.sum.us":{"value":633.288}},"transaction":{"type":"request","name":"GET /"},"span":{"type":"external","subtype":"http"},"timestamp": 1496170422281000}}
`

	req, _ := http.NewRequest("POST", serverURL.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-ndjson")
	ipAddress := "220.244.41.16"
	req.Header.Set("X-Forwarded-For", ipAddress)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	result := systemtest.Elasticsearch.ExpectMinDocs(t, 2, "traces-apm*,metrics-apm*", estest.TermsQuery{
		Field:  "processor.event",
		Values: []interface{}{"transaction", "metric"},
	})
	systemtest.ApproveEvents(
		t, t.Name(), result.Hits.Hits,
		// RUM timestamps are set by the server based on the time the payload is received.
		"@timestamp", "timestamp.us",
		// RUM events have the source port recorded, and in the tests it will be dynamic
		"source.port",
		// Do not assert the exact contents of the location field since they may change
		// slightly depending on the IP lookup.
		"client.geo.location",
	)
}

func TestRUMAllowServiceNames(t *testing.T) {
	srv := apmservertest.NewUnstartedServerTB(t)
	srv.Config.AgentAuth.SecretToken = "abc123"
	srv.Config.RUM = &apmservertest.RUMConfig{
		Enabled: true,
	}
	srv.Config.AgentAuth.Anonymous = &apmservertest.AnonymousAuthConfig{
		Enabled:      true,
		AllowService: []string{"allowed"},
	}
	err := srv.Start()
	require.NoError(t, err)

	// Send a RUM transaction where the service name in metadata is allowed,
	// but is overridden in the transaction event's context with a disallowed
	// service name.
	reqBody := strings.NewReader(`
{"metadata":{"service":{"name":"allowed","version":"1.0.0","agent":{"name":"rum-js","version":"0.0.0"}}}}
{"transaction":{"trace_id":"x","id":"y","type":"z","duration":0,"span_count":{"started":1},"context":{"service":{"name":"disallowed"}}}}
`[1:])
	req, _ := http.NewRequest("POST", srv.URL+"/intake/v2/rum/events", reqBody)
	req.Header.Add("Content-Type", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(respBody))
	assert.Equal(t, `{"accepted":0,"errors":[{"message":"unauthorized: anonymous access not permitted for service \"disallowed\""}]}`+"\n", string(respBody))
}

func TestRUMRateLimit(t *testing.T) {
	srv := apmservertest.NewUnstartedServerTB(t)
	srv.Config.AgentAuth.SecretToken = "abc123" // enable auth & rate limiting
	srv.Config.RUM = &apmservertest.RUMConfig{Enabled: true}
	srv.Config.AgentAuth.Anonymous = &apmservertest.AnonymousAuthConfig{
		Enabled: true,
		RateLimit: &apmservertest.RateLimitConfig{
			IPLimit: 2,

			// Set the event limit to less than 10 (the batch size)
			// to immediately return 429 rather than waiting until
			// another batch can be processed.
			EventLimit: 5,
		},
	}
	err := srv.Start()
	require.NoError(t, err)

	sendEvents := func(ip string, n int) error {
		body := bytes.NewBufferString(`{"metadata":{"service":{"name":"allowed","version":"1.0.0","agent":{"name":"rum-js","version":"0.0.0"}}}}` + "\n")
		for i := 0; i < n; i++ {
			body.WriteString(`{"transaction":{"trace_id":"x","id":"y","type":"z","duration":0,"span_count":{"started":1},"context":{"service":{"name":"foo"}}}}` + "\n")
		}

		req, _ := http.NewRequest("POST", srv.URL+"/intake/v2/rum/events?verbose=true", body)
		req.Header.Add("Content-Type", "application/x-ndjson")
		req.Header.Add("X-Forwarded-For", ip)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusAccepted {
			return fmt.Errorf("%s (%s)", resp.Status, strings.TrimSpace(string(respBody)))
		}
		return nil
	}

	// The configured event rate limit is multiplied by 3 for the initial burst. Check that
	// for the configured IP limit (2), we can handle 3*event_limit without being rate limited.
	err = sendEvents("10.11.12.13", 3*srv.Config.AgentAuth.Anonymous.RateLimit.EventLimit)
	assert.NoError(t, err)

	// Sending the events over multiple requests should have the same outcome.
	for i := 0; i < 3; i++ {
		err = sendEvents("10.11.12.14", srv.Config.AgentAuth.Anonymous.RateLimit.EventLimit)
		assert.NoError(t, err)
	}

	// The rate limiter cache only has space for 2 IPs, so the 3rd one reuses an existing
	// limiter, which will have already been exhausted.
	err = sendEvents("10.11.12.15", 10)
	require.Error(t, err)

	// The exact error differs, depending on whether rate limiting was applied at the request
	// level, or at the event stream level. Either could occur.
	assert.Regexp(t, `429 Too Many Requests .*`, err.Error())
}

func TestRUMCORS(t *testing.T) {
	// Check that CORS configuration is effective. More specific behaviour is unit tested.
	srv := apmservertest.NewUnstartedServerTB(t)
	srv.Config.RUM = &apmservertest.RUMConfig{
		Enabled:      true,
		AllowOrigins: []string{"blue"},
		AllowHeaders: []string{"stick", "door"},
	}
	err := srv.Start()
	require.NoError(t, err)

	req, _ := http.NewRequest("OPTIONS", srv.URL+"/intake/v2/rum/events", nil)
	req.Header.Set("Origin", "blue")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "blue", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "POST, OPTIONS", resp.Header.Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "stick, door, Content-Type, Content-Encoding, Accept", resp.Header.Get("Access-Control-Allow-Headers"))
}

func TestRUMRoutingIntegration(t *testing.T) {
	// This test asserts that the events that are coming from the RUM JS agent
	// are sent to the appropriate datastream.
	systemtest.CleanupElasticsearch(t)
	srv := apmservertest.NewUnstartedServerTB(t)
	srv.Config.RUM = &apmservertest.RUMConfig{Enabled: true}
	srv.Config.Kibana.Enabled = false
	err := srv.Start()
	require.NoError(t, err)

	body, err := os.ReadFile(filepath.Join("..", "testdata", "intake-v3", "rum_events.ndjson"))
	require.NoError(t, err)

	retry := func() {
		req, err := http.NewRequest("POST", srv.URL+"/intake/v3/rum/events", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Add("Content-Type", "application/x-ndjson")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
	}

	result := systemtest.Elasticsearch.ExpectSourcemapError(t, "traces-apm.rum*", retry, nil, false)
	systemtest.ApproveEvents(
		t, t.Name(), result.Hits.Hits, "@timestamp", "timestamp.us",
		"source.port", "source.ip", "client",
	)
}
