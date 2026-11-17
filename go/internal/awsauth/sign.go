// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package awsauth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Sign signs req for service in region with c at now: SigV4 in the
// Authorization header, over the host, X-Amz-Date, the session token's
// header when c has one, every header req carries already, and the
// SHA-256 of body, the request's payload. It sets those headers itself.
// An API of AWS beside RDS's connect (Route 53's, ADR 0045) takes it.
func Sign(req *http.Request, body []byte, c Credentials, region, service string, now time.Time) {
	date := now.UTC().Format("20060102")
	stamp := now.UTC().Format("20060102T150405Z")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	req.Header.Set("X-Amz-Date", stamp)
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for k, vs := range req.Header {
		trimmed := make([]string, len(vs))
		for i, v := range vs {
			trimmed[i] = strings.Join(strings.Fields(v), " ")
		}
		headers[strings.ToLower(k)] = strings.Join(trimmed, ",")
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	sum := sha256.Sum256(body)
	canonical := req.Method + "\n" + path + "\n" + canonicalQuery(req) + "\n" + canonHeaders.String() + "\n" + signed +
		"\n" + hex.EncodeToString(sum[:])
	reqSum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(reqSum[:])
	key := mac([]byte("AWS4"+c.SecretAccessKey), date)
	key = mac(key, region)
	key = mac(key, service)
	key = mac(key, "aws4_request")
	sig := hex.EncodeToString(mac(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKeyID+"/"+scope+", SignedHeaders="+signed+
		", Signature="+sig)
}

// canonicalQuery is req's query as SigV4 signs it: every name and value
// escaped, sorted by name, then by value.
func canonicalQuery(req *http.Request) string {
	q := req.URL.Query()
	var parts []string
	for k, vs := range q {
		for _, v := range vs {
			parts = append(parts, escape(k)+"="+escape(v))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}
