// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	networkv1 "github.com/gardenlinux/feos/go/gen/feos/network/v1"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// nameService is feosd's NameService over tables in memory, with feosd's
// rules (network-service names.rs, name_store.rs): a put replaces the
// table whatever its version, with its names in lower case, sorted, and
// their addresses as a sorted set in canonical form; an update applies a
// delta from the version the node has to another; a table a pod on the
// node names is not deleted. Node.Resolve answers a pod's name as feosd's
// resolver does. With Config.CheckNameTables a pod that names a table the
// node does not have fails at its bring-up, as on feosd; with
// Config.NoNameService the node serves none, as a feosd from before.
type nameService struct {
	networkv1.UnimplementedNameServiceServer
	n *Node

	mu     sync.Mutex
	tables map[string]*networkv1.NameTable
}

func newNameService(n *Node) *nameService {
	return &nameService{n: n, tables: map[string]*networkv1.NameTable{}}
}

func checkTableID(id string) error {
	if !validGroupID(id) {
		return status.Errorf(codes.InvalidArgument, "invalid name table ID %q: 1 to 64 letters, digits, \"-\", \"_\" and \".\"", id)
	}
	return nil
}

// validLabel: one label of a DNS name, 1 to 63 letters, digits and "-",
// "-" neither first nor last.
func validLabel(name string) bool {
	if len(name) == 0 || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func label(name string) (string, error) {
	if !validLabel(name) {
		return "", status.Errorf(codes.InvalidArgument, "name %q is not one label of a DNS name", name)
	}
	return strings.ToLower(name), nil
}

// normalizeEntry checks an entry and writes its name in lower case and
// its addresses as a sorted set in canonical form.
func normalizeEntry(e *networkv1.NameEntry) (*networkv1.NameEntry, error) {
	name, err := label(e.GetName())
	if err != nil {
		return nil, err
	}
	set := map[netip.Addr]bool{}
	for _, a := range e.GetAddresses() {
		ip, err := netip.ParseAddr(a)
		if err != nil || ip.Zone() != "" {
			return nil, status.Errorf(codes.InvalidArgument, "address %q of name %s is not an IP address", a, name)
		}
		set[ip] = true
	}
	if len(set) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "name %s has no address", name)
	}
	ips := slices.SortedFunc(maps.Keys(set), func(a, b netip.Addr) int { return a.Compare(b) })
	out := &networkv1.NameEntry{Name: name}
	for _, ip := range ips {
		out.Addresses = append(out.Addresses, ip.String())
	}
	return out, nil
}

// normalizeTable checks a table and writes its entries sorted by name.
func normalizeTable(in *networkv1.NameTable) (*networkv1.NameTable, error) {
	t := proto.Clone(in).(*networkv1.NameTable)
	if err := checkTableID(t.GetId()); err != nil {
		return nil, err
	}
	for _, inc := range t.GetIncludes() {
		if !validGroupID(inc) {
			return nil, status.Errorf(codes.InvalidArgument, "name table %s includes %q, which is no table ID", t.GetId(), inc)
		}
	}
	entries := map[string]*networkv1.NameEntry{}
	for _, e := range t.GetEntries() {
		ne, err := normalizeEntry(e)
		if err != nil {
			return nil, err
		}
		if entries[ne.Name] != nil {
			return nil, status.Errorf(codes.InvalidArgument, "name table %s has name %s twice", t.GetId(), ne.Name)
		}
		entries[ne.Name] = ne
	}
	t.Entries = nil
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		t.Entries = append(t.Entries, entries[name])
	}
	return t, nil
}

func (s *nameService) PutNameTable(_ context.Context, req *networkv1.PutNameTableRequest) (*networkv1.PutNameTableResponse, error) {
	if req.GetTable() == nil {
		return nil, status.Error(codes.InvalidArgument, "no name table")
	}
	t, err := normalizeTable(req.GetTable())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tables[t.Id] = t
	return &networkv1.PutNameTableResponse{}, nil
}

func (s *nameService) UpdateNameTable(_ context.Context, req *networkv1.UpdateNameTableRequest) (*networkv1.UpdateNameTableResponse, error) {
	id := req.GetId()
	if err := checkTableID(id); err != nil {
		return nil, err
	}
	if req.GetVersion() == req.GetFromVersion() {
		return nil, status.Errorf(codes.InvalidArgument, "version %d is the version the update starts from", req.GetVersion())
	}
	put := map[string]*networkv1.NameEntry{}
	for _, e := range req.GetPutEntries() {
		ne, err := normalizeEntry(e)
		if err != nil {
			return nil, err
		}
		if put[ne.Name] != nil {
			return nil, status.Errorf(codes.InvalidArgument, "the update puts a name twice in name table %s", id)
		}
		put[ne.Name] = ne
	}
	remove := map[string]bool{}
	for _, name := range req.GetRemoveNames() {
		l, err := label(name)
		if err != nil {
			return nil, err
		}
		if put[l] != nil {
			return nil, status.Errorf(codes.InvalidArgument, "name %s is both put and removed", l)
		}
		remove[l] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tables[id]
	if t == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "name table %s is not on the node", id)
	}
	if t.Version != req.GetFromVersion() {
		return nil, status.Errorf(codes.FailedPrecondition, "name table %s is at version %d, not %d", id, t.Version, req.GetFromVersion())
	}
	entries := map[string]*networkv1.NameEntry{}
	for _, e := range t.Entries {
		if !remove[e.Name] {
			entries[e.Name] = e
		}
	}
	maps.Copy(entries, put)
	u := proto.Clone(t).(*networkv1.NameTable)
	u.Version, u.Entries = req.GetVersion(), nil
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		u.Entries = append(u.Entries, entries[name])
	}
	s.tables[id] = u
	return &networkv1.UpdateNameTableResponse{}, nil
}

func (s *nameService) DeleteNameTable(_ context.Context, req *networkv1.DeleteNameTableRequest) (*networkv1.DeleteNameTableResponse, error) {
	id := req.GetId()
	inUse := s.n.pods.namesTable(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tables[id] == nil {
		return nil, status.Errorf(codes.NotFound, "name table %s is not on the node", id)
	}
	if inUse {
		return nil, status.Errorf(codes.FailedPrecondition, "a pod on the node names name table %s", id)
	}
	delete(s.tables, id)
	return &networkv1.DeleteNameTableResponse{}, nil
}

func (s *nameService) GetNameTable(_ context.Context, req *networkv1.GetNameTableRequest) (*networkv1.NameTable, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tables[req.GetId()]
	if t == nil {
		return nil, status.Errorf(codes.NotFound, "name table %s is not on the node", req.GetId())
	}
	return proto.Clone(t).(*networkv1.NameTable), nil
}

func (s *nameService) ListNameTables(context.Context, *networkv1.ListNameTablesRequest) (*networkv1.ListNameTablesResponse, error) {
	return &networkv1.ListNameTablesResponse{Tables: s.list()}, nil
}

// list returns the tables, sorted by ID.
func (s *nameService) list() []*networkv1.NameTable {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*networkv1.NameTable, 0, len(s.tables))
	for _, t := range s.tables {
		out = append(out, proto.Clone(t).(*networkv1.NameTable))
	}
	slices.SortFunc(out, func(a, b *networkv1.NameTable) int { return strings.Compare(a.Id, b.Id) })
	return out
}

// lookup is feosd's: the addresses of name in the first of ids that holds
// it, a table's own names before those of the tables it includes.
func (s *nameService) lookup(ids []string, name string) ([]string, bool) {
	name = strings.ToLower(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	find := func(id string) ([]string, bool) {
		if t := s.tables[id]; t != nil {
			for _, e := range t.Entries {
				if e.Name == name {
					return slices.Clone(e.Addresses), true
				}
			}
		}
		return nil, false
	}
	for _, id := range ids {
		if a, ok := find(id); ok {
			return a, true
		}
		if t := s.tables[id]; t != nil {
			for _, inc := range t.Includes {
				if a, ok := find(inc); ok {
					return a, true
				}
			}
		}
	}
	return nil, false
}

// attachError is a pod's bring-up's failure when its name tables are not
// all on the node (Config.CheckNameTables), as feosd's attach of its
// network fails.
func (s *nameService) attachError(ids []string) error {
	if !s.n.cfg.CheckNameTables || s.n.cfg.NoNameService {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var missing []string
	for _, id := range ids {
		if s.tables[id] == nil {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the node does not have the name tables %s", strings.Join(missing, ", "))
	}
	return nil
}

// NameTables returns the node's name tables, sorted by ID.
func (n *Node) NameTables() []*networkv1.NameTable { return n.names.list() }

// Resolve answers name, one label, for the pod as feosd's resolver does
// from the name tables the pod's spec names: the name's addresses of both
// families, and whether the tables hold it (false: feosd would ask
// upstream). feosd answers a name whose addresses include the pod's own
// with loopback (ADR 0035); this gives the table's addresses, which is
// what ferund puts there. A node without the NameService answers
// nothing.
func (n *Node) Resolve(pod podid.ID, name string) ([]string, bool) {
	info, err := n.pods.get(pod.String())
	if err != nil || n.cfg.NoNameService {
		return nil, false
	}
	return n.names.lookup(info.GetSpec().GetNetwork().GetNameTables(), name)
}
