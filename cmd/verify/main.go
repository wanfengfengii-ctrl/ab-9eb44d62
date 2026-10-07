// Command verify runs the live smoke suite against a running schema
// registry API: it waits for readiness, then exercises publishing,
// evolution rules, idempotency, conflict handling and concurrency.
// Exit code 0 means every check passed.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	base := flag.String("base-url", envOr("API_BASE_URL", "http://localhost:8080"), "base URL of the schema registry API")
	wait := flag.Duration("wait", 90*time.Second, "how long to wait for the API to become ready")
	flag.Parse()

	c := &checker{base: *base, client: &http.Client{Timeout: 10 * time.Second}}

	fmt.Printf("[verify] waiting for API at %s to become ready (up to %s)\n", *base, wait)
	if !c.waitReady(*wait) {
		fmt.Println("[verify] FATAL: API did not become ready in time")
		os.Exit(1)
	}
	fmt.Println("[verify] API is ready, running smoke checks")

	runID := fmt.Sprintf("%d%04d", time.Now().Unix(), rand.Intn(10000))
	subj := func(name string) string { return "smoke." + runID + "." + name }

	c.testFirstVersionChain(subj("chain"))
	c.testEvolutionAndTombstones(subj("evolve"))
	c.testIdempotency(subj("idem"))
	c.testRequestValidation(subj("invalid"))
	c.testConcurrentSuccessors(subj("race"))
	c.testConcurrentReplay(subj("race-replay"))

	fmt.Printf("\n[verify] %d checks, %d failures\n", c.checks, c.failures)
	if c.failures > 0 {
		os.Exit(1)
	}
	fmt.Println("[verify] SMOKE TESTS PASSED")
}

// ---------------------------------------------------------------------------
// check plumbing
// ---------------------------------------------------------------------------

type checker struct {
	base     string
	client   *http.Client
	checks   int
	failures int
}

func (c *checker) ok(name string, cond bool, detail ...any) {
	c.checks++
	if cond {
		fmt.Printf("  PASS  %s\n", name)
		return
	}
	c.failures++
	fmt.Printf("  FAIL  %s: %s\n", name, fmt.Sprint(detail...))
}

func (c *checker) waitReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := c.client.Get(c.base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

type apiResp struct {
	status int
	body   map[string]any
	raw    []byte
	header http.Header
}

func (c *checker) do(method, path string, payload any) apiResp {
	var rdr io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			panic(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		panic(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return apiResp{status: -1, body: map[string]any{"transportError": err.Error()}, header: http.Header{}}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return apiResp{status: resp.StatusCode, body: m, raw: raw, header: resp.Header}
}

func (c *checker) publish(subject string, payload map[string]any) apiResp {
	return c.do(http.MethodPost, "/api/schemas/"+subject+"/versions", payload)
}

func (c *checker) get(subject string) apiResp {
	return c.do(http.MethodGet, "/api/schemas/"+subject, nil)
}

func fld(name string, num int, wireType string) map[string]any {
	return map[string]any{"name": name, "number": num, "wireType": wireType}
}

func pubBody(requestID string, version int, fields ...map[string]any) map[string]any {
	return map[string]any{"requestId": requestID, "version": version, "fields": fields}
}

// ---------------------------------------------------------------------------
// response decoding helpers
// ---------------------------------------------------------------------------

func errField(r apiResp, key string) (any, bool) {
	e, ok := r.body["error"].(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := e[key]
	return v, ok
}

func errCode(r apiResp) string {
	if v, ok := errField(r, "code"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func errInt(r apiResp, key string) (int, bool) {
	if v, ok := errField(r, key); ok {
		if f, ok := v.(float64); ok {
			return int(f), true
		}
	}
	return 0, false
}

func intList(v any) []int {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]int, 0, len(arr))
	for _, item := range arr {
		if f, ok := item.(float64); ok {
			out = append(out, int(f))
		}
	}
	sort.Ints(out)
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// smoke scenarios
// ---------------------------------------------------------------------------

// Version chain: first version must be 1, successors must be current+1.
func (c *checker) testFirstVersionChain(subject string) {
	fmt.Println("[scenario] version chain")

	r := c.publish(subject, pubBody("chain-1", 2, fld("a", 1, "VARINT")))
	c.ok("first version other than 1 is rejected with 409",
		r.status == http.StatusConflict, "got status ", r.status, " body ", string(r.raw))
	c.ok("rejection uses stable code VERSION_CONFLICT",
		errCode(r) == "VERSION_CONFLICT", "got ", errCode(r))
	cv, hasCV := errInt(r, "currentVersion")
	c.ok("rejection reports currentVersion 0", hasCV && cv == 0, "got ", r.body["error"])

	r = c.publish(subject, pubBody("chain-2", 1, fld("a", 1, "VARINT")))
	c.ok("first version 1 is accepted with 201", r.status == http.StatusCreated, "got ", r.status, " ", string(r.raw))

	r = c.publish(subject, pubBody("chain-3", 3, fld("a", 1, "VARINT")))
	c.ok("skipping a version is rejected with 409 VERSION_CONFLICT",
		r.status == http.StatusConflict && errCode(r) == "VERSION_CONFLICT", "got ", r.status, " ", errCode(r))
	cv, _ = errInt(r, "currentVersion")
	c.ok("version conflict reports currentVersion 1", cv == 1, "got ", r.body["error"])

	g := c.get(subject)
	c.ok("failed publishes leave currentVersion at 1",
		g.status == http.StatusOK && g.body["currentVersion"] == float64(1), "got ", g.status, " ", string(g.raw))
}

// Evolution rules: rename/retype rejected, deletion tombstones numbers,
// reserved numbers can neither be reused nor revived.
func (c *checker) testEvolutionAndTombstones(subject string) {
	fmt.Println("[scenario] evolution rules and reserved numbers")

	r := c.publish(subject, pubBody("evo-1", 1,
		fld("alpha", 1, "VARINT"), fld("beta", 2, "FIXED32"), fld("gamma", 3, "LENGTH_DELIMITED")))
	c.ok("v1 published", r.status == http.StatusCreated, "got ", r.status, " ", string(r.raw))

	// Evolve: keep alpha, drop beta (number 2), keep gamma, add delta.
	r = c.publish(subject, pubBody("evo-2", 2,
		fld("alpha", 1, "VARINT"), fld("gamma", 3, "LENGTH_DELIMITED"), fld("delta", 4, "FIXED64")))
	c.ok("v2 with deletion and addition published", r.status == http.StatusCreated, "got ", r.status, " ", string(r.raw))

	g := c.get(subject)
	c.ok("deleted field number 2 is tombstoned in reservedNumbers",
		g.status == http.StatusOK && equalInts(intList(g.body["reservedNumbers"]), []int{2}),
		"got ", string(g.raw))

	// Rename of an existing number.
	r = c.publish(subject, pubBody("evo-3", 3,
		fld("alphaRenamed", 1, "VARINT"), fld("gamma", 3, "LENGTH_DELIMITED"), fld("delta", 4, "FIXED64")))
	c.ok("renaming an existing number is rejected with 422",
		r.status == http.StatusUnprocessableEntity, "got ", r.status, " ", string(r.raw))
	c.ok("rename rejection uses stable code SCHEMA_EVOLUTION_VIOLATION",
		errCode(r) == "SCHEMA_EVOLUTION_VIOLATION", "got ", errCode(r))
	cv, _ := errInt(r, "currentVersion")
	vn, hasVN := errInt(r, "violatingNumber")
	c.ok("rename rejection reports currentVersion 2 and violatingNumber 1",
		cv == 2 && hasVN && vn == 1, "got ", r.body["error"])

	// Wire type change of an existing number.
	r = c.publish(subject, pubBody("evo-4", 3,
		fld("alpha", 1, "FIXED32"), fld("gamma", 3, "LENGTH_DELIMITED"), fld("delta", 4, "FIXED64")))
	c.ok("changing a wire type is rejected with 422 SCHEMA_EVOLUTION_VIOLATION",
		r.status == http.StatusUnprocessableEntity && errCode(r) == "SCHEMA_EVOLUTION_VIOLATION",
		"got ", r.status, " ", errCode(r))
	vn, _ = errInt(r, "violatingNumber")
	c.ok("wire type rejection reports violatingNumber 1", vn == 1, "got ", r.body["error"])

	// Reusing a tombstoned number for a new field.
	r = c.publish(subject, pubBody("evo-5", 3,
		fld("alpha", 1, "VARINT"), fld("gamma", 3, "LENGTH_DELIMITED"), fld("delta", 4, "FIXED64"), fld("zeta", 2, "VARINT")))
	c.ok("reusing a reserved number is rejected with 422",
		r.status == http.StatusUnprocessableEntity && errCode(r) == "SCHEMA_EVOLUTION_VIOLATION",
		"got ", r.status, " ", errCode(r))
	vn, _ = errInt(r, "violatingNumber")
	c.ok("reserved-number rejection reports violatingNumber 2", vn == 2, "got ", r.body["error"])

	// Reviving the deleted field with its original name and type.
	r = c.publish(subject, pubBody("evo-6", 3,
		fld("alpha", 1, "VARINT"), fld("beta", 2, "FIXED32"), fld("gamma", 3, "LENGTH_DELIMITED"), fld("delta", 4, "FIXED64")))
	c.ok("reviving a deleted field (un-reserving) is rejected with 422",
		r.status == http.StatusUnprocessableEntity && errCode(r) == "SCHEMA_EVOLUTION_VIOLATION",
		"got ", r.status, " ", errCode(r))

	// None of the failed requests may have changed current schema or tombstones.
	g = c.get(subject)
	c.ok("failed evolutions leave currentVersion at 2",
		g.status == http.StatusOK && g.body["currentVersion"] == float64(2), "got ", string(g.raw))
	c.ok("failed evolutions leave reservedNumbers unchanged",
		g.status == http.StatusOK && equalInts(intList(g.body["reservedNumbers"]), []int{2}), "got ", string(g.raw))
	fields, _ := g.body["fields"].([]any)
	c.ok("failed evolutions leave current fields unchanged", len(fields) == 3, "got ", string(g.raw))
}

// Idempotency: same requestId + same content replays the original result;
// same requestId + different content conflicts.
func (c *checker) testIdempotency(subject string) {
	fmt.Println("[scenario] idempotency")

	body := pubBody("idem-1", 1, fld("a", 1, "VARINT"), fld("b", 2, "FIXED64"))
	r1 := c.publish(subject, body)
	c.ok("initial publish accepted", r1.status == http.StatusCreated, "got ", r1.status, " ", string(r1.raw))

	r2 := c.publish(subject, body)
	c.ok("identical retry replays the original 201 result",
		r2.status == http.StatusCreated && bytes.Equal(r1.raw, r2.raw),
		"got ", r2.status, " ", string(r2.raw))
	c.ok("replay is marked with X-Idempotent-Replay header",
		r2.header.Get("X-Idempotent-Replay") == "true", "headers ", r2.header)

	g := c.get(subject)
	c.ok("replay did not create a new version",
		g.status == http.StatusOK && g.body["currentVersion"] == float64(1), "got ", string(g.raw))

	r3 := c.publish(subject, pubBody("idem-1", 1, fld("a", 1, "VARINT"), fld("c", 3, "VARINT")))
	c.ok("same requestId with different content conflicts with 409 REQUEST_ID_CONFLICT",
		r3.status == http.StatusConflict && errCode(r3) == "REQUEST_ID_CONFLICT",
		"got ", r3.status, " ", errCode(r3))

	// Evolve to v2, then replay the v1 request again: still the original result.
	r4 := c.publish(subject, pubBody("idem-2", 2, fld("a", 1, "VARINT")))
	c.ok("evolution to v2 accepted", r4.status == http.StatusCreated, "got ", r4.status, " ", string(r4.raw))
	r5 := c.publish(subject, body)
	c.ok("original request still replays after later versions",
		r5.status == http.StatusCreated && bytes.Equal(r1.raw, r5.raw), "got ", r5.status, " ", string(r5.raw))
	g = c.get(subject)
	c.ok("replay after evolution leaves currentVersion at 2",
		g.status == http.StatusOK && g.body["currentVersion"] == float64(2), "got ", string(g.raw))
}

// Request validation: malformed payloads get a stable 400 code.
func (c *checker) testRequestValidation(subject string) {
	fmt.Println("[scenario] request validation")

	r := c.publish(subject, pubBody("val-1", 1, fld("a", 1, "STRING")))
	c.ok("unknown wire type is rejected with 400 INVALID_REQUEST",
		r.status == http.StatusBadRequest && errCode(r) == "INVALID_REQUEST", "got ", r.status, " ", errCode(r))

	r = c.publish(subject, map[string]any{"version": 1, "fields": []any{}})
	c.ok("missing requestId is rejected with 400 INVALID_REQUEST",
		r.status == http.StatusBadRequest && errCode(r) == "INVALID_REQUEST", "got ", r.status, " ", errCode(r))

	r = c.publish(subject, pubBody("val-2", 1, fld("a", 1, "VARINT"), fld("b", 1, "FIXED32")))
	c.ok("duplicate field number is rejected with 400 INVALID_REQUEST",
		r.status == http.StatusBadRequest && errCode(r) == "INVALID_REQUEST", "got ", r.status, " ", errCode(r))

	g := c.get(subject)
	c.ok("subject with only failed requests stays unpublished (404)",
		g.status == http.StatusNotFound && errCode(g) == "SUBJECT_NOT_FOUND", "got ", g.status, " ", errCode(g))
}

// Concurrency: racing publishes of the same successor version succeed once.
func (c *checker) testConcurrentSuccessors(subject string) {
	fmt.Println("[scenario] concurrent successor versions")

	r := c.publish(subject, pubBody("race-0", 1, fld("a", 1, "VARINT")))
	c.ok("v1 published", r.status == http.StatusCreated, "got ", r.status, " ", string(r.raw))

	const racers = 8
	statuses := make([]int, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res := c.publish(subject, pubBody(fmt.Sprintf("race-%d", i+1), 2,
				fld("a", 1, "VARINT"), fld(fmt.Sprintf("f%d", i), 10+i, "FIXED32")))
			statuses[i] = res.status
		}(i)
	}
	wg.Wait()

	created, conflicts := 0, 0
	for _, s := range statuses {
		switch s {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		}
	}
	c.ok("exactly one of 8 concurrent v2 publishes succeeded",
		created == 1, "got ", created, " successes out of ", statuses)
	c.ok("all losing races got 409",
		conflicts == racers-1, "got ", conflicts, " conflicts out of ", statuses)

	g := c.get(subject)
	versions := intList(g.body["versions"])
	c.ok("a single current version 2 exists after the race",
		g.status == http.StatusOK && g.body["currentVersion"] == float64(2) && equalInts(versions, []int{1, 2}),
		"got ", string(g.raw))
}

// Concurrency + idempotency: racing identical requests all replay the one commit.
func (c *checker) testConcurrentReplay(subject string) {
	fmt.Println("[scenario] concurrent identical requests")

	const racers = 8
	body := pubBody("replay-1", 1, fld("a", 1, "VARINT"))
	raws := make([][]byte, racers)
	statuses := make([]int, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res := c.publish(subject, body)
			statuses[i] = res.status
			raws[i] = res.raw
		}(i)
	}
	wg.Wait()

	created := 0
	identical := true
	for i, s := range statuses {
		if s == http.StatusCreated {
			created++
		}
		if !bytes.Equal(raws[0], raws[i]) {
			identical = false
		}
	}
	c.ok("all 8 identical concurrent requests returned 201",
		created == racers, "got statuses ", statuses)
	c.ok("all identical concurrent requests returned the same body",
		identical, "bodies diverged")

	g := c.get(subject)
	c.ok("only one version was committed",
		g.status == http.StatusOK && g.body["currentVersion"] == float64(1) && equalInts(intList(g.body["versions"]), []int{1}),
		"got ", string(g.raw))
}
