// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeValkey answers GET, INCRBY and EXPIRE over RESP from a map.
type fakeValkey struct {
	ln   net.Listener
	mu   sync.Mutex
	vals map[string]int64
	ttl  map[string]string
}

func newFakeValkey(t *testing.T) *fakeValkey {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeValkey{ln: ln, vals: map[string]int64{}, ttl: map[string]string{}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeValkey) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil || line[0] != '*' {
		return
	}
	n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
	args := make([]string, n)
	for i := range args {
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
		a, _ := r.ReadString('\n')
		args[i] = strings.TrimSuffix(a, "\r\n")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "GET":
		v, ok := f.vals[args[1]]
		if !ok {
			io.WriteString(c, "$-1\r\n")
			return
		}
		s := strconv.FormatInt(v, 10)
		fmt.Fprintf(c, "$%d\r\n%s\r\n", len(s), s)
	case "INCRBY":
		d, _ := strconv.ParseInt(args[2], 10, 64)
		f.vals[args[1]] += d
		fmt.Fprintf(c, ":%d\r\n", f.vals[args[1]])
	case "EXPIRE":
		f.ttl[args[1]] = args[2]
		io.WriteString(c, ":1\r\n")
	default:
		io.WriteString(c, "-ERR unknown command\r\n")
	}
}

func (f *fakeValkey) get(k string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vals[k]
}

// A call's cost: plain input, output with reasoning, cache reads and
// writes, each at its price per million tokens, in micro-dollars rounded
// up.
func TestCostMicro(t *testing.T) {
	p := prices{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}
	for _, c := range []struct {
		u    usage
		want int64
	}{
		{usage{}, 0},
		{usage{Prompt: 1_000_000}, 3_000_000},
		{usage{Completion: 1_000}, 15_000},
		{usage{Prompt: 1_000, CacheRead: 600, CacheWrite: 300, Completion: 10}, 100*3 + 600*0.3 + 300*3.75 + 10*15},
		{usage{Prompt: 1}, 3},
		{usage{Prompt: 10, CacheRead: 20}, 6}, // never a negative plain part
	} {
		if got := costMicro(c.u, p); got != c.want {
			t.Errorf("costMicro(%+v) = %d, want %d", c.u, got, c.want)
		}
	}
	if got := costMicro(usage{Prompt: 1}, prices{Input: 0.1}); got != 1 {
		t.Errorf("a fraction of a micro-dollar rounds up to 1, got %d", got)
	}
}

// The settings: none without BUDGET_URL; every number required with it.
func TestLoadBudget(t *testing.T) {
	set := func(env map[string]string) {
		for _, k := range []string{"BUDGET_URL", "BUDGET_MONTHLY_USD", "PRICE_INPUT_USD", "PRICE_OUTPUT_USD",
			"PRICE_CACHE_READ_USD", "PRICE_CACHE_WRITE_USD"} {
			t.Setenv(k, env[k])
		}
	}
	set(nil)
	if b, err := loadBudget(); b != nil || err != nil {
		t.Fatalf("without BUDGET_URL: %v, %v", b, err)
	}
	full := map[string]string{"BUDGET_URL": "redis://127.64.0.3:6379/0", "BUDGET_MONTHLY_USD": "25",
		"PRICE_INPUT_USD": "3", "PRICE_OUTPUT_USD": "15", "PRICE_CACHE_READ_USD": "0.3", "PRICE_CACHE_WRITE_USD": "3.75"}
	set(full)
	b, err := loadBudget()
	if err != nil || b.addr != "127.64.0.3:6379" || b.capMicro != 25_000_000 || b.prices.CacheWrite != 3.75 {
		t.Fatalf("budget %+v, %v", b, err)
	}
	for _, k := range []string{"BUDGET_MONTHLY_USD", "PRICE_OUTPUT_USD"} {
		bad := map[string]string{}
		for kk, v := range full {
			bad[kk] = v
		}
		bad[k] = "lots"
		set(bad)
		if _, err := loadBudget(); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=lots: %v", k, err)
		}
	}
	full["BUDGET_URL"] = "http://x"
	set(full)
	if _, err := loadBudget(); err == nil {
		t.Error("an http URL was taken")
	}
}

// A chat counts every round's cost into the month's counter, and once the
// counter reaches the cap, a chat is refused before it calls the model.
func TestChatBudget(t *testing.T) {
	kv := newFakeValkey(t)
	calls := 0
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":100,"+
			"\"prompt_tokens_details\":{\"cached_tokens\":400,\"cache_write_tokens\":100}}}\n\n")
		fmt.Fprintf(w, "data: %s\n\n", DONE)
	}))
	defer llm.Close()
	now := time.Date(2026, 11, 17, 20, 0, 0, 0, time.UTC)
	b := &budget{addr: kv.ln.Addr().String(), capMicro: 6000,
		prices: prices{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}, now: func() time.Time { return now }}
	cfg := config{BaseURL: llm.URL, Model: "m", SystemPromptFile: "/nonexistent", Budget: b}
	one := int64(500*3 + 400*0.3 + 100*3.75 + 100*15) // 3495
	key := "fechat:spend:2026-11"

	chatTurn(t, cfg, []Message{{Role: "user", Content: "x"}})
	if calls != 1 || kv.get(key) != one {
		t.Fatalf("after one chat: %d calls, %d micro-USD; want 1, %d", calls, kv.get(key), one)
	}
	if kv.ttl[key] == "" {
		t.Error("the month's counter has no expiry")
	}
	chatTurn(t, cfg, []Message{{Role: "user", Content: "x"}}) // below the cap before: runs, ends above it
	if calls != 2 || kv.get(key) != 2*one {
		t.Fatalf("after two chats: %d calls, %d", calls, kv.get(key))
	}
	rec := httptest.NewRecorder()
	handleChat(cfg)(rec, httptest.NewRequest(http.MethodPost, "/api/v1/chat",
		strings.NewReader(`{"messages":[{"role":"user","content":"x"}],"conversation_id":"c"}`)))
	if calls != 2 || !strings.Contains(rec.Body.String(), "budget for the model is spent") {
		t.Fatalf("over the cap: %d calls, answer %q", calls, rec.Body.String())
	}
	now = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) // a new month, a new counter
	chatTurn(t, cfg, []Message{{Role: "user", Content: "x"}})
	if calls != 3 || kv.get("fechat:spend:2026-12") != one {
		t.Fatalf("next month: %d calls, %d", calls, kv.get("fechat:spend:2026-12"))
	}

	// A counter that cannot be read refuses the chat: no call unchecked.
	kv.ln.Close()
	rec = httptest.NewRecorder()
	handleChat(cfg)(rec, httptest.NewRequest(http.MethodPost, "/api/v1/chat",
		strings.NewReader(`{"messages":[{"role":"user","content":"x"}],"conversation_id":"c"}`)))
	if calls != 3 || !strings.Contains(rec.Body.String(), "cannot check its budget") {
		t.Fatalf("without the counter: %d calls, answer %q", calls, rec.Body.String())
	}
}
