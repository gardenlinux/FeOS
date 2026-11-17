// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package awsauth signs in to Amazon RDS with IAM: an authentication
// token, a SigV4-presigned URL of the action connect that PostgreSQL takes
// as the password for 15 minutes, made from the credentials the platform
// hands the process. Those are an EKS Pod Identity's
// (AWS_CONTAINER_CREDENTIALS_FULL_URI and
// AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE), which expire and are fetched
// again before they do, or static keys (AWS_ACCESS_KEY_ID,
// AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN). Standard library only: the
// token is one presigned GET, the credentials one GET.
package awsauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Credentials are an AWS access key, with its session token when it is a
// temporary one, and when it expires (zero: never).
type Credentials struct {
	AccessKeyID, SecretAccessKey, SessionToken string
	Expires                                    time.Time
}

// Source hands out credentials, fetching a Pod Identity's again five
// minutes before they expire.
type Source struct {
	client *http.Client
	getenv func(string) string

	mu  sync.Mutex
	cur Credentials
}

// NewSource reads the credentials from the environment as the AWS SDKs
// do, static keys first, then a Pod Identity's endpoint.
func NewSource() *Source {
	return &Source{client: &http.Client{Timeout: 10 * time.Second}, getenv: os.Getenv}
}

// refreshBefore is how long before their expiry credentials are fetched
// again.
const refreshBefore = 5 * time.Minute

// Get returns current credentials.
func (s *Source) Get(ctx context.Context) (Credentials, error) {
	if id, secret := s.getenv("AWS_ACCESS_KEY_ID"), s.getenv("AWS_SECRET_ACCESS_KEY"); id != "" && secret != "" {
		return Credentials{AccessKeyID: id, SecretAccessKey: secret, SessionToken: s.getenv("AWS_SESSION_TOKEN")}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur.AccessKeyID != "" && time.Until(s.cur.Expires) > refreshBefore {
		return s.cur, nil
	}
	c, err := s.fetch(ctx)
	if err != nil {
		return Credentials{}, err
	}
	s.cur = c
	return c, nil
}

// fetch asks the Pod Identity agent for the role's credentials.
func (s *Source) fetch(ctx context.Context) (Credentials, error) {
	uri, file := s.getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI"), s.getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE")
	if uri == "" || file == "" {
		return Credentials{}, errors.New("no AWS credentials: neither AWS_ACCESS_KEY_ID nor a Pod Identity (AWS_CONTAINER_CREDENTIALS_FULL_URI)")
	}
	token, err := os.ReadFile(file)
	if err != nil {
		return Credentials{}, fmt.Errorf("the Pod Identity's token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return Credentials{}, err
	}
	req.Header.Set("Authorization", strings.TrimSpace(string(token)))
	resp, err := s.client.Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("the Pod Identity's credentials: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return Credentials{}, fmt.Errorf("the Pod Identity's credentials: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		Token           string `json:"Token"`
		Expiration      time.Time
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Credentials{}, fmt.Errorf("the Pod Identity's credentials: %w", err)
	}
	if out.AccessKeyID == "" || out.SecretAccessKey == "" {
		return Credentials{}, errors.New("the Pod Identity answered no access key")
	}
	return Credentials{AccessKeyID: out.AccessKeyID, SecretAccessKey: out.SecretAccessKey,
		SessionToken: out.Token, Expires: out.Expiration}, nil
}

// emptyHash is the SHA-256 of an empty payload, a presigned GET's.
const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// RDSToken is the IAM authentication token of user at the RDS endpoint
// host:port in region, made at now with c: valid for 15 minutes, for a
// connection's first message, not for its life.
func RDSToken(c Credentials, region, host string, port uint16, user string, now time.Time) string {
	endpoint := fmt.Sprintf("%s:%d", host, port)
	date := now.UTC().Format("20060102")
	stamp := now.UTC().Format("20060102T150405Z")
	scope := date + "/" + region + "/rds-db/aws4_request"
	q := map[string]string{
		"Action":              "connect",
		"DBUser":              user,
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    c.AccessKeyID + "/" + scope,
		"X-Amz-Date":          stamp,
		"X-Amz-Expires":       "900",
		"X-Amz-SignedHeaders": "host",
	}
	if c.SessionToken != "" {
		q["X-Amz-Security-Token"] = c.SessionToken
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = escape(k) + "=" + escape(q[k])
	}
	query := strings.Join(parts, "&")
	canonical := "GET\n/\n" + query + "\nhost:" + endpoint + "\n\nhost\n" + emptyHash
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	key := mac([]byte("AWS4"+c.SecretAccessKey), date)
	key = mac(key, region)
	key = mac(key, "rds-db")
	key = mac(key, "aws4_request")
	sig := hex.EncodeToString(mac(key, toSign))
	return endpoint + "/?" + query + "&X-Amz-Signature=" + sig
}

func mac(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}

// escape is SigV4's URI encoding: every byte but the unreserved ones.
func escape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}
