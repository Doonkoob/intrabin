package dns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/controlplane"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/security"
	"github.com/intrabin/intrabin/internal/storage"
)

// Fencer proves the right to perform external writes and returns (term, leader ID).
type Fencer func(ctx context.Context) (uint64, string, error)

// Service performs DNS changes safely: plan → snapshot → conflict check → fence → durable
// "dispatching" record → fence → provider call → durable outcome → read-back verification.
type Service struct {
	Store *storage.Store
	CP    *controlplane.CP
	Box   *security.SecretBox
	Fence Fencer
	Log   *slog.Logger
	// APIBase overrides provider base URLs ("cloudflare", "arvancloud"); test and demo use only.
	APIBase map[string]string
	// VerifyDelay is the wait before re-reading a record after an uncertain write (default 2 s).
	VerifyDelay time.Duration

	mu    sync.Mutex
	zones map[string]zoneCache
}

type zoneCache struct {
	zones []Zone
	at    time.Time
}

var (
	// ErrConflict means the record changed at the provider since the plan was made.
	ErrConflict = errors.New("the record changed at the provider since this change was planned")
	// ErrAmbiguous means several records match and a record ID is required.
	ErrAmbiguous = errors.New("several records match; choose the record to change")
	// ErrNoChange means the record already has the desired state.
	ErrNoChange = errors.New("the record already has this value")
)

// Credentials are the decrypted provider credentials.
type Credentials struct {
	APIToken string `json:"api_token,omitempty"` // Cloudflare
	APIKey   string `json:"api_key,omitempty"`   // ArvanCloud
}

func credAAD(id string) []byte { return []byte("dnsprovider:" + id) }

// SealCredentials encrypts provider credentials with the cluster data key.
func SealCredentials(box *security.SecretBox, providerID string, c Credentials) ([]byte, error) {
	b, _ := json.Marshal(c)
	return box.Seal(b, credAAD(providerID))
}

// ProviderTypes lists supported provider kinds.
var ProviderTypes = []string{"cloudflare", "arvancloud"}

// Client builds a provider client for a stored integration.
func (s *Service) Client(p *model.DNSProvider) (Provider, error) {
	b, err := s.Box.Open(p.CredentialBlob, credAAD(p.ID))
	if err != nil {
		return nil, errors.New("cannot decrypt the provider credentials")
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return s.NewClient(p.Type, c)
}

// NewClient builds a provider client from plaintext credentials (used to verify before saving).
func (s *Service) NewClient(kind string, c Credentials) (Provider, error) {
	switch kind {
	case "cloudflare":
		if c.APIToken == "" {
			return nil, fmt.Errorf("%w: an API token is required", ErrInvalid)
		}
		return NewCloudflare(s.APIBase["cloudflare"], c.APIToken), nil
	case "arvancloud":
		if c.APIKey == "" {
			return nil, fmt.Errorf("%w: an API key is required", ErrInvalid)
		}
		return NewArvan(s.APIBase["arvancloud"], c.APIKey), nil
	}
	return nil, fmt.Errorf("%w: unknown provider type %q", ErrInvalid, kind)
}

func (s *Service) load(ctx context.Context, providerID string) (*model.DNSProvider, Provider, error) {
	p, err := s.Store.GetDNSProvider(ctx, providerID)
	if err != nil {
		return nil, nil, err
	}
	c, err := s.Client(p)
	return p, c, err
}

// Zones lists the provider's zones (cached for 30 s).
func (s *Service) Zones(ctx context.Context, providerID string, fresh bool) ([]Zone, error) {
	s.mu.Lock()
	if s.zones == nil {
		s.zones = map[string]zoneCache{}
	}
	zc, ok := s.zones[providerID]
	s.mu.Unlock()
	if ok && !fresh && time.Since(zc.at) < 30*time.Second {
		return zc.zones, nil
	}
	_, c, err := s.load(ctx, providerID)
	if err != nil {
		return nil, err
	}
	zs, err := c.Zones(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.zones[providerID] = zoneCache{zones: zs, at: time.Now()}
	s.mu.Unlock()
	return zs, nil
}

// Zone resolves a zone by ID or name.
func (s *Service) Zone(ctx context.Context, providerID, zoneRef string) (Zone, error) {
	for _, fresh := range []bool{false, true} {
		zs, err := s.Zones(ctx, providerID, fresh)
		if err != nil {
			return Zone{}, err
		}
		for _, z := range zs {
			if z.ID == zoneRef || z.Name == strings.ToLower(strings.TrimSuffix(zoneRef, ".")) {
				return z, nil
			}
		}
	}
	return Zone{}, fmt.Errorf("zone %q: %w", zoneRef, ErrNotFound)
}

// Records lists a zone's records.
func (s *Service) Records(ctx context.Context, providerID, zoneRef string) (Zone, []Record, error) {
	_, c, err := s.load(ctx, providerID)
	if err != nil {
		return Zone{}, nil, err
	}
	z, err := s.Zone(ctx, providerID, zoneRef)
	if err != nil {
		return Zone{}, nil, err
	}
	rs, err := c.Records(ctx, z)
	return z, rs, err
}

func toModel(r *Record) *model.DNSRecord {
	if r == nil {
		return nil
	}
	return &model.DNSRecord{ID: r.ID, Type: r.Type, Name: r.Name, Content: r.Content, TTL: r.TTL, Proxied: r.Proxied}
}

func inputModel(in RecordInput) *model.DNSRecord {
	return &model.DNSRecord{Type: in.Type, Name: in.Name, Content: in.Content, TTL: in.TTL, Proxied: in.Proxied}
}

func modelInput(r *model.DNSRecord) RecordInput {
	return RecordInput{Type: r.Type, Name: r.Name, Content: r.Content, TTL: r.TTL, Proxied: r.Proxied}
}

// ChangeRequest describes a desired change.
type ChangeRequest struct {
	ProviderID string      `json:"provider_id"`
	Zone       string      `json:"zone"`      // zone ID or name
	Action     string      `json:"action"`    // upsert | delete
	RecordID   string      `json:"record_id"` // optional: the exact record to change
	Record     RecordInput `json:"record"`    // desired state (upsert) or selector (delete: type + name)
	ExpectID   string      `json:"-"`         // internal: change ID to use (idempotency)
	Source     string      `json:"-"`
}

// Plan is a computed change, shown to the user before it is applied.
type Plan struct {
	ProviderID string           `json:"provider_id"`
	Zone       Zone             `json:"zone"`
	Action     string           `json:"action"` // create | update | delete
	Before     *model.DNSRecord `json:"before,omitempty"`
	After      *model.DNSRecord `json:"after,omitempty"`
	NoChange   bool             `json:"no_change"`
	Warnings   []string         `json:"warnings,omitempty"`
}

// PlanChange computes what a request would do, against the provider's current records.
func (s *Service) PlanChange(ctx context.Context, req ChangeRequest) (*Plan, error) {
	z, rs, err := s.Records(ctx, req.ProviderID, req.Zone)
	if err != nil {
		return nil, err
	}
	return s.plan(req, z, rs)
}

func (s *Service) plan(req ChangeRequest, z Zone, rs []Record) (*Plan, error) {
	in := req.Record
	if req.Action == "delete" && req.RecordID != "" && in.Type == "" {
		if cur := Find(rs, req.RecordID); cur != nil {
			in = RecordInput{Type: cur.Type, Name: cur.Name, Content: cur.Content, TTL: cur.TTL, Proxied: cur.Proxied}
		}
	}
	if req.Action == "delete" && in.Content == "" {
		in.Content = "0.0.0.0" // selector only; content is not used for delete validation
		if in.Type != "A" {
			in.Content = "x.invalid"
		}
		if in.Type == "TXT" {
			in.Content = "x"
		}
		if in.Type == "AAAA" {
			in.Content = "::"
		}
	}
	if err := Validate(&in, z.Name); err != nil {
		return nil, err
	}
	p := &Plan{ProviderID: req.ProviderID, Zone: z}
	var cur *Record
	if req.Action == "create" {
		// explicit create (used by snapshot restore): never touches existing records
		for _, r := range FindByName(rs, in.Type, in.Name) {
			if Matches(&r, in) {
				p.Action, p.After, p.NoChange = "create", inputModel(in), true
				return p, nil
			}
		}
	} else if req.RecordID != "" {
		cur = Find(rs, req.RecordID)
		if cur == nil {
			return nil, fmt.Errorf("record %s: %w", req.RecordID, ErrNotFound)
		}
		if !cur.Editable {
			return nil, fmt.Errorf("%w: %s records cannot be changed here", ErrUnsupported, cur.Type)
		}
	} else {
		same := FindByName(rs, in.Type, in.Name)
		switch len(same) {
		case 0:
		case 1:
			cur = &same[0]
		default:
			return nil, ErrAmbiguous
		}
		if in.Type == "CNAME" || len(same) == 0 {
			for _, r := range rs {
				if r.Name == in.Name && r.Type != in.Type && (in.Type == "CNAME" || r.Type == "CNAME") {
					p.Warnings = append(p.Warnings, fmt.Sprintf("A %s record already exists at %s; most providers reject a CNAME next to other records.", r.Type, in.Name))
				}
			}
		}
	}
	switch req.Action {
	case "create":
		p.Action, p.After = "create", inputModel(in)
	case "upsert":
		p.After = inputModel(in)
		if cur == nil {
			p.Action = "create"
		} else {
			p.Action, p.Before = "update", toModel(cur)
			if cur.Type != in.Type {
				return nil, fmt.Errorf("%w: the record type cannot be changed; delete and create instead", ErrInvalid)
			}
			p.NoChange = Matches(cur, in)
		}
	case "delete":
		if cur == nil {
			return nil, fmt.Errorf("%s %s: %w", in.Type, in.Name, ErrNotFound)
		}
		p.Action, p.Before = "delete", toModel(cur)
	default:
		return nil, fmt.Errorf("%w: action must be upsert, create or delete", ErrInvalid)
	}
	return p, nil
}

// Snapshot stores the zone's current records.
func (s *Service) Snapshot(ctx context.Context, providerID, zoneRef, reason, actor string) (*model.DNSSnapshot, error) {
	z, rs, err := s.Records(ctx, providerID, zoneRef)
	if err != nil {
		return nil, err
	}
	return s.saveSnapshot(ctx, providerID, z, rs, reason, actor)
}

func (s *Service) saveSnapshot(ctx context.Context, providerID string, z Zone, rs []Record, reason, actor string) (*model.DNSSnapshot, error) {
	sn := &model.DNSSnapshot{ID: uuid.NewString(), ProviderID: providerID, ZoneID: z.ID, ZoneName: z.Name, Reason: reason, CreatedBy: actor, CreatedAt: time.Now().UTC()}
	for i := range rs {
		sn.Records = append(sn.Records, *toModel(&rs[i]))
	}
	sn.RecordCount = len(sn.Records)
	if err := s.CP.SaveDNSSnapshot(ctx, sn); err != nil {
		return nil, err
	}
	return sn, nil
}

func (s *Service) fence(ctx context.Context) (uint64, string, error) {
	if s.Fence == nil {
		return 0, "", nil
	}
	return s.Fence(ctx)
}

// Apply plans and applies a change. If plan is non-nil, the provider's current record must still
// match plan.Before (otherwise ErrConflict). Every attempt is recorded in the change history.
func (s *Service) Apply(ctx context.Context, req ChangeRequest, expected *Plan, actor string) (*model.DNSChange, error) {
	if req.ExpectID != "" {
		if prev, err := s.Store.GetDNSChange(ctx, req.ExpectID); err == nil {
			return prev, nil // idempotent: this change was already attempted
		}
	}
	_, c, err := s.load(ctx, req.ProviderID)
	if err != nil {
		return nil, err
	}
	z, rs, err := s.Records(ctx, req.ProviderID, req.Zone)
	if err != nil {
		return nil, err
	}
	p, err := s.plan(req, z, rs)
	if err != nil {
		return nil, err
	}
	if expected != nil && !sameRecord(expected.Before, p.Before) {
		return nil, ErrConflict
	}
	if p.NoChange {
		return nil, ErrNoChange
	}
	term, leader, err := s.fence(ctx)
	if err != nil {
		return nil, err
	}
	sel := subject(p)
	reason := "Before " + p.Action + " " + sel.Type + " " + sel.Name
	sn, err := s.saveSnapshot(ctx, req.ProviderID, z, rs, reason, actor)
	if err != nil {
		return nil, err
	}
	id := req.ExpectID
	if id == "" {
		id = uuid.NewString()
	}
	ch := &model.DNSChange{ID: id, ProviderID: req.ProviderID, ZoneID: z.ID, ZoneName: z.Name, Action: p.Action, RecordType: sel.Type, RecordName: sel.Name,
		Before: p.Before, After: p.After, Status: model.DNSChangeDispatching, SnapshotID: sn.ID, Source: firstNonEmpty(req.Source, "manual"),
		FenceTerm: term, FenceLeader: leader, Actor: actor, CreatedAt: time.Now().UTC()}
	if p.Before != nil {
		ch.RecordID = p.Before.ID
	}
	if err := s.CP.SaveDNSChange(ctx, ch); err != nil {
		return nil, err
	}
	if t2, l2, err := s.fence(ctx); err != nil || t2 != term || l2 != leader {
		ch.Status, ch.Error = model.DNSChangeFailed, "not dispatched: leadership could not be re-verified"
		s.finish(ctx, ch)
		return ch, controlplane.ErrNotLeader
	}
	var res *Record
	switch p.Action {
	case "create":
		res, err = c.Create(ctx, z, modelInput(p.After))
	case "update":
		cur := Find(rs, p.Before.ID)
		res, err = c.Update(ctx, z, cur, modelInput(p.After))
	case "delete":
		err = c.Delete(ctx, z, p.Before.ID)
	}
	if ctx.Err() != nil {
		return ch, ctx.Err() // shutting down: the next leader reconciles from the provider state
	}
	switch {
	case err == nil:
		ch.Status = model.DNSChangeApplied
		if res != nil {
			ch.RecordID = res.ID
		}
		ch.Verified, ch.Error = s.verify(ctx, c, z, ch)
	case definite(err):
		ch.Status, ch.Error = model.DNSChangeFailed, safeErr(err)
	default:
		// The write may or may not have been applied: read the zone back to find out.
		ch.Status, ch.Error = model.DNSChangeUnknown, safeErr(err)
		s.resolveFromState(ctx, c, z, ch)
	}
	s.finish(ctx, ch)
	return ch, nil
}

// subject returns the record a plan is about (the new state, or the deleted record).
func subject(p *Plan) *model.DNSRecord {
	if p.After != nil {
		return p.After
	}
	return p.Before
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func sameRecord(a, b *model.DNSRecord) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.ID == b.ID && Matches(&Record{Type: a.Type, Name: a.Name, Content: a.Content, TTL: a.TTL, Proxied: a.Proxied}, modelInput(b)) && a.TTL == b.TTL
}

// definite reports whether an error proves the write was not applied.
func definite(err error) bool {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Status >= 400 && pe.Status < 500 // the provider rejected the request
	}
	return errors.Is(err, ErrAuth) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrRateLimited) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrUnsupported)
}

// SafeError returns a message for err that is safe to store and show (no URLs, no credentials).
func SafeError(err error) string {
	switch {
	case errors.Is(err, ErrConflict), errors.Is(err, ErrAmbiguous), errors.Is(err, ErrNoChange):
		return err.Error()
	case errors.Is(err, storage.ErrNotFound):
		return "the DNS integration does not exist"
	case errors.Is(err, controlplane.ErrNotLeader):
		return "this node is not the active controller"
	}
	return safeErr(err)
}

func safeErr(err error) string {
	var pe *ProviderError
	var te *TransportError
	switch {
	case errors.As(err, &pe):
		return pe.Error()
	case errors.As(err, &te):
		return "the request failed before a response was received"
	case errors.Is(err, ErrAuth), errors.Is(err, ErrNotFound), errors.Is(err, ErrRateLimited), errors.Is(err, ErrInvalid), errors.Is(err, ErrUnsupported):
		return err.Error()
	}
	return "unexpected error"
}

func (s *Service) finish(ctx context.Context, ch *model.DNSChange) {
	now := time.Now().UTC()
	ch.FinishedAt = &now
	if ch.Status == model.DNSChangeDispatching {
		ch.FinishedAt = nil
	}
	if err := s.CP.SaveDNSChange(ctx, ch); err != nil && s.Log != nil {
		s.Log.Warn("dns: could not record change outcome", "change_id", ch.ID, "err", err)
	}
}

// state classifies the provider's current state for a change: "applied" (desired state present),
// "before" (unchanged) or "other".
func state(rs []Record, ch *model.DNSChange) string {
	switch ch.Action {
	case "create":
		for _, r := range rs {
			if Matches(&r, modelInput(ch.After)) {
				return "applied"
			}
		}
		return "before"
	case "update":
		cur := Find(rs, ch.Before.ID)
		if cur != nil && Matches(cur, modelInput(ch.After)) {
			return "applied"
		}
		if cur != nil && sameRecord(toModel(cur), ch.Before) {
			return "before"
		}
		return "other"
	case "delete":
		if Find(rs, ch.Before.ID) == nil {
			return "applied"
		}
		return "before"
	}
	return "other"
}

func (s *Service) verify(ctx context.Context, c Provider, z Zone, ch *model.DNSChange) (bool, string) {
	rs, err := c.Records(ctx, z)
	if err != nil {
		return false, "applied; read-back verification failed: " + safeErr(err)
	}
	if state(rs, ch) != "applied" {
		return false, "applied, but the record read back differs from the requested value"
	}
	return true, ""
}

// resolveFromState settles an uncertain change by reading the zone back after a short delay.
func (s *Service) resolveFromState(ctx context.Context, c Provider, z Zone, ch *model.DNSChange) {
	d := s.VerifyDelay
	if d == 0 {
		d = 2 * time.Second
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(d):
	}
	rs, err := c.Records(ctx, z)
	if err != nil {
		return // stays unknown_outcome
	}
	switch state(rs, ch) {
	case "applied":
		ch.Status, ch.Verified = model.DNSChangeReconciledApplied, true
		for _, r := range rs {
			if ch.Action == "create" && Matches(&r, modelInput(ch.After)) {
				ch.RecordID = r.ID
			}
		}
	case "before":
		ch.Status = model.DNSChangeReconciledNotApplied
	}
}

// ReconcilePending resolves changes left "dispatching" by a previous leader (or a crash) by
// reading the provider state. It never re-sends the write.
func (s *Service) ReconcilePending(ctx context.Context, olderThan time.Duration) {
	chs, err := s.Store.ListDNSChanges(ctx, storage.DNSChangeFilter{Statuses: []string{model.DNSChangeDispatching}, Limit: 50})
	if err != nil {
		return
	}
	for _, ch := range chs {
		if time.Since(ch.CreatedAt) < olderThan {
			continue
		}
		if _, _, err := s.fence(ctx); err != nil {
			return
		}
		s.resolveOne(ctx, ch)
	}
}

// ResolveChange returns the change with this ID, first settling it from the provider state if
// it was left "dispatching". found is false when no change was ever recorded under the ID, which
// proves the provider was never called.
func (s *Service) ResolveChange(ctx context.Context, id string) (*model.DNSChange, bool, error) {
	ch, err := s.Store.GetDNSChange(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if ch.Status == model.DNSChangeDispatching {
		if _, _, err := s.fence(ctx); err != nil {
			return ch, true, err
		}
		s.resolveOne(ctx, ch)
	}
	return ch, true, nil
}

func (s *Service) resolveOne(ctx context.Context, ch *model.DNSChange) {
	_, c, err := s.load(ctx, ch.ProviderID)
	if err != nil {
		return
	}
	rs, err := c.Records(ctx, Zone{ID: ch.ZoneID, Name: ch.ZoneName})
	if err != nil {
		return
	}
	switch state(rs, ch) {
	case "applied":
		ch.Status, ch.Verified = model.DNSChangeReconciledApplied, true
	case "before":
		ch.Status = model.DNSChangeReconciledNotApplied
	default:
		ch.Status = model.DNSChangeUnknown
	}
	ch.Error = strings.TrimSpace(ch.Error + " The node that started this change stopped before recording its result; the outcome was determined by reading the zone back.")
	if ch.Status == model.DNSChangeUnknown {
		ch.Error = "The node that started this change stopped before recording its result, and the record now has neither the old nor the requested value. Check the zone and reconcile manually."
	}
	s.finish(ctx, ch)
}

// ---- revert and restore ----

// RevertRequest returns the change request that undoes ch.
func RevertRequest(ch *model.DNSChange) (ChangeRequest, error) {
	applied := ch.Status == model.DNSChangeApplied || ch.Status == model.DNSChangeReconciledApplied
	if !applied {
		return ChangeRequest{}, fmt.Errorf("%w: only applied changes can be reverted", ErrInvalid)
	}
	req := ChangeRequest{ProviderID: ch.ProviderID, Zone: ch.ZoneID, Source: "revert:" + ch.ID}
	switch ch.Action {
	case "create":
		req.Action, req.RecordID = "delete", ch.RecordID
	case "update":
		req.Action, req.RecordID, req.Record = "upsert", ch.RecordID, modelInput(ch.Before)
	case "delete":
		req.Action, req.Record = "upsert", modelInput(ch.Before)
	}
	return req, nil
}

// RestorePlan lists the changes that bring editable records back to a snapshot.
func (s *Service) RestorePlan(ctx context.Context, sn *model.DNSSnapshot, deleteExtra bool) ([]ChangeRequest, []*Plan, error) {
	z, rs, err := s.Records(ctx, sn.ProviderID, sn.ZoneID)
	if err != nil {
		return nil, nil, err
	}
	var reqs []ChangeRequest
	var plans []*Plan
	add := func(req ChangeRequest) error {
		req.Source = "restore:" + sn.ID
		p, err := s.plan(req, z, rs)
		if err != nil {
			return err
		}
		if !p.NoChange {
			reqs, plans = append(reqs, req), append(plans, p)
		}
		return nil
	}
	inSnap := map[string]bool{}
	for _, r := range sn.Records {
		if !isEditable(r.Type) {
			continue
		}
		inSnap[r.ID] = true
		req := ChangeRequest{ProviderID: sn.ProviderID, Zone: z.ID, Action: "create", Record: modelInput(&r)}
		if Find(rs, r.ID) != nil {
			req.Action, req.RecordID = "upsert", r.ID
		} else {
			for _, x := range FindByName(rs, r.Type, r.Name) {
				if Matches(&x, modelInput(&r)) {
					inSnap[x.ID] = true // same record re-created under another ID: keep it
				}
			}
		}
		if err := add(req); err != nil {
			return nil, nil, err
		}
	}
	if deleteExtra {
		for _, r := range rs {
			if r.Editable && !inSnap[r.ID] {
				if err := add(ChangeRequest{ProviderID: sn.ProviderID, Zone: z.ID, Action: "delete", RecordID: r.ID}); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	return reqs, plans, nil
}

// ReconcileManual records an operator's verified outcome for a change whose outcome is unknown.
func (s *Service) ReconcileManual(ctx context.Context, id string, applied bool, actor, note string) (*model.DNSChange, error) {
	ch, err := s.Store.GetDNSChange(ctx, id)
	if err != nil {
		return nil, err
	}
	if ch.Status != model.DNSChangeUnknown {
		return nil, fmt.Errorf("%w: only changes with an unknown outcome can be reconciled", ErrInvalid)
	}
	ch.Status = model.DNSChangeReconciledNotApplied
	if applied {
		ch.Status = model.DNSChangeReconciledApplied
	}
	ch.Error = strings.TrimSpace(ch.Error + " Reconciled by " + actor + ": " + note)
	s.finish(ctx, ch)
	return ch, nil
}
