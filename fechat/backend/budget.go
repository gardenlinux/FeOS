// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The model's budget: what FeChat spends on the LLM, by calendar month
// (UTC), and a cap on it. The LLM's API answers tokens, not money, so
// every call's cost is its usage times the prices FeChat is given per
// million tokens: input, output (reasoning included), cache reads and
// cache writes. The month's sum is a counter in a Valkey (the environment's
// addon), in micro-dollars, so that it holds across restarts and
// instances; a call starts only while the month's sum is below the cap.
// Calls already running finish and are counted, so a month can end a
// little above it.
//
//	BUDGET_URL              redis://host:port of the Valkey; unset: no budget
//	BUDGET_MONTHLY_USD      the cap per month, e.g. 25
//	PRICE_INPUT_USD         per million input tokens (not read from or
//	PRICE_OUTPUT_USD        written to the cache), per million output tokens,
//	PRICE_CACHE_READ_USD    per million cache-read and cache-write tokens
//	PRICE_CACHE_WRITE_USD
//
// The Valkey speaks plain RESP: FeRun's TLS binding (ADR 0039) carries it
// to the addon with the service's certificate.

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// usage is one call's tokens as the API reports them in the stream's last
// chunk: prompt includes the cached and the cache-written tokens,
// completion the reasoning tokens.
type usage struct {
	Prompt, Completion, CacheRead, CacheWrite int
}

// prices are USD per million tokens.
type prices struct {
	Input, Output, CacheRead, CacheWrite float64
}

// costMicro is what u costs at p, in micro-dollars, rounded up.
func costMicro(u usage, p prices) int64 {
	plain := u.Prompt - u.CacheRead - u.CacheWrite
	if plain < 0 {
		plain = 0
	}
	// USD per million tokens is micro-dollars per token.
	c := float64(plain)*p.Input + float64(u.Completion)*p.Output +
		float64(u.CacheRead)*p.CacheRead + float64(u.CacheWrite)*p.CacheWrite
	return int64(math.Ceil(c))
}

// budget is the month's counter and its cap.
type budget struct {
	addr     string // host:port
	capMicro int64
	prices   prices
	now      func() time.Time
}

// errBudgetSpent refuses a call: the month's sum reached the cap.
var errBudgetSpent = errors.New("the model's budget for this month is spent")

// loadBudget reads the budget's settings; nil without BUDGET_URL.
func loadBudget() (*budget, error) {
	raw := os.Getenv("BUDGET_URL")
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "redis" && u.Scheme != "valkey") || u.Host == "" {
		return nil, fmt.Errorf("BUDGET_URL must be redis://host:port, got %q", raw)
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), "6379")
	}
	num := func(k string) (float64, error) {
		v, err := strconv.ParseFloat(os.Getenv(k), 64)
		if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("%s must be a number of USD, got %q", k, os.Getenv(k))
		}
		return v, nil
	}
	b := &budget{addr: addr, now: time.Now}
	monthly, err := num("BUDGET_MONTHLY_USD")
	if err != nil {
		return nil, err
	}
	b.capMicro = int64(monthly * 1e6)
	for k, p := range map[string]*float64{"PRICE_INPUT_USD": &b.prices.Input, "PRICE_OUTPUT_USD": &b.prices.Output,
		"PRICE_CACHE_READ_USD": &b.prices.CacheRead, "PRICE_CACHE_WRITE_USD": &b.prices.CacheWrite} {
		if *p, err = num(k); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// key is the counter of the month of t.
func (b *budget) key(t time.Time) string { return "fechat:spend:" + t.UTC().Format("2006-01") }

// check returns nil while the month's sum is below the cap,
// errBudgetSpent when it is not, and another error when the counter
// cannot be read: then the call does not start either.
func (b *budget) check(ctx context.Context) error {
	r, err := b.do(ctx, "GET", b.key(b.now()))
	if err != nil {
		return fmt.Errorf("the model's budget cannot be read: %w", err)
	}
	spent, _ := r.(int64)
	if s, ok := r.(string); ok {
		spent, _ = strconv.ParseInt(s, 10, 64)
	}
	if spent >= b.capMicro {
		return errBudgetSpent
	}
	return nil
}

// add counts a call's cost into the month's sum, which expires after two
// months.
func (b *budget) add(ctx context.Context, u usage) (int64, error) {
	k := b.key(b.now())
	r, err := b.do(ctx, "INCRBY", k, strconv.FormatInt(costMicro(u, b.prices), 10))
	if err != nil {
		return 0, err
	}
	if _, err := b.do(ctx, "EXPIRE", k, strconv.Itoa(62*24*3600)); err != nil {
		return 0, err
	}
	total, _ := r.(int64)
	return total, nil
}

// do sends one command over a connection of its own and returns its
// reply: an int64, a string, nil, or the server's error.
func (b *budget) do(ctx context.Context, args ...string) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", b.addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	var cmd strings.Builder
	fmt.Fprintf(&cmd, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&cmd, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := io.WriteString(conn, cmd.String()); err != nil {
		return nil, err
	}
	return readReply(bufio.NewReader(conn))
}

// readReply reads one RESP reply of the kinds GET, INCRBY and EXPIRE
// answer.
func readReply(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(line, "\r\n")
	if line == "" {
		return nil, errors.New("an empty reply")
	}
	switch body := line[1:]; line[0] {
	case '+':
		return body, nil
	case '-':
		return nil, errors.New(body)
	case ':':
		return strconv.ParseInt(body, 10, 64)
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil || n < -1 {
			return nil, fmt.Errorf("a bad bulk length %q", body)
		}
		if n == -1 {
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	default:
		return nil, fmt.Errorf("an unexpected reply %q", line)
	}
}
