package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jimutt/spov/internal/credproxy"
	"github.com/jimutt/spov/internal/routedcred"
	"github.com/jimutt/spov/internal/store"
)

// NoCredentials is the configuration's narrowing that attaches none:
// `sandbox.credentials: [none]`.
//
// @decision(DL-075)
const NoCredentials = "none"

// MaxNeeds bounds the hosts a launch may say it needs.
const MaxNeeds = 16

// candidateRoute is a routed credential a launch could attach, and the
// tier it came from.
//
// @decision(DL-075)
type candidateRoute struct {
	HeldRoute
	Tier Tier
}

// record is what the session keeps of it: never the secret.
func (c candidateRoute) record() store.SessionRoutedCredential {
	return store.SessionRoutedCredential{
		Name: c.Name, Tier: string(c.Tier), Hosts: c.Hosts, Paths: c.Paths,
		Scheme: c.Scheme, User: c.User, Header: c.Header, Preset: c.Preset,
	}
}

// candidates are the personal routed credentials, then the organisation's.
// A personal credential takes a host, and a name, from the organisation:
// an organisation credential that shares either is left out, with a note.
//
// @decision(DL-075) @decision(DL-074)
func candidates(personal, org []HeldRoute) ([]candidateRoute, []string) {
	var out []candidateRoute
	var notes []string
	hosts, names := map[string]string{}, map[string]bool{}
	for _, h := range personal {
		out = append(out, candidateRoute{HeldRoute: h, Tier: TierUser})
		names[h.Name] = true
		for _, host := range h.Hosts {
			if _, taken := hosts[host]; !taken {
				hosts[host] = h.Name
			}
		}
	}
	for _, h := range org {
		if names[h.Name] {
			notes = append(notes, fmt.Sprintf("the organisation's routed credential %s is left out: you hold one of the same name", h.Name))
			continue
		}
		taken := ""
		for _, host := range h.Hosts {
			if mine, ok := hosts[host]; ok {
				taken = fmt.Sprintf("your %s takes %s", mine, host)
				break
			}
		}
		if taken != "" {
			notes = append(notes, fmt.Sprintf("the organisation's routed credential %s is left out: %s", h.Name, taken))
			continue
		}
		out = append(out, candidateRoute{HeldRoute: h, Tier: TierOrg})
	}
	return out, notes
}

// errRoutedRequest is a launch's routed credentials that cannot be
// attached as asked: an unknown name, or two on one host.
var errRoutedRequest = errors.New("routed credentials")

// attachRoutes decides which candidates a launch attaches: the launch's
// own list exactly, else every candidate attached always, narrowed by
// the configuration's list, which never adds one. The notes say what was
// left out and why.
//
// @decision(DL-075)
func attachRoutes(cands []candidateRoute, req store.RoutedRequest) ([]candidateRoute, []string, error) {
	var out []candidateRoute
	var notes []string
	byName := func(name string) (candidateRoute, bool) {
		i := slices.IndexFunc(cands, func(c candidateRoute) bool { return c.Name == name })
		if i < 0 {
			return candidateRoute{}, false
		}
		return cands[i], true
	}
	switch {
	case req.None:
	case len(req.Names) > 0:
		for _, name := range req.Names {
			c, ok := byName(name)
			if !ok {
				return nil, nil, fmt.Errorf("%w: you hold none named %s", errRoutedRequest, name)
			}
			if !slices.ContainsFunc(out, func(x candidateRoute) bool { return x.Name == name }) {
				out = append(out, c)
			}
		}
	case slices.Equal(req.Preferred, []string{NoCredentials}):
	default:
		for _, c := range cands {
			if c.Attach == routedcred.AttachAlways && (len(req.Preferred) == 0 || slices.Contains(req.Preferred, c.Name)) {
				out = append(out, c)
			}
		}
		for _, name := range req.Preferred {
			if _, ok := byName(name); !ok {
				notes = append(notes, fmt.Sprintf("the configuration names routed credential %s, which you do not hold", name))
			}
		}
	}
	owner := map[string]string{}
	for _, c := range out {
		for _, h := range c.Hosts {
			if other, ok := owner[h]; ok {
				return nil, nil, fmt.Errorf("%w: %s and %s both route %s; launch with --credentials naming one", errRoutedRequest, other, c.Name, h)
			}
			owner[h] = c.Name
		}
	}
	return out, notes, nil
}

// uncoveredNeeds are the needed hosts no attached credential routes.
//
// @decision(DL-075)
func uncoveredNeeds(attached []candidateRoute, needs []string) []string {
	var out []string
	for _, n := range needs {
		if !slices.ContainsFunc(attached, func(c candidateRoute) bool { return slices.Contains(c.Hosts, n) }) {
			out = append(out, n)
		}
	}
	return out
}

// routedRequest is the stored form of a request's routed fields, hosts
// lowercased.
//
// @decision(DL-075)
func routedRequest(names []string, none bool, preferred, needs []string) store.RoutedRequest {
	lower := make([]string, len(needs))
	for i, h := range needs {
		lower[i] = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	}
	if len(lower) == 0 {
		lower = nil
	}
	return store.RoutedRequest{Names: names, None: none, Preferred: preferred, Needs: lower}
}

// checkRoutedRequest validates a request's routed fields before a row
// is written.
//
// @decision(DL-075)
func checkRoutedRequest(r store.RoutedRequest) error {
	if r.None && len(r.Names) > 0 {
		return errors.New("credentials and no_credentials cannot both be set")
	}
	for _, list := range [][]string{r.Names, r.Preferred} {
		if len(list) > routedcred.MaxPerUser+routedcred.MaxPerOrg {
			return errors.New("too many routed credentials named")
		}
		for _, n := range list {
			if n != NoCredentials && !credproxy.ValidRouteName(n) {
				return fmt.Errorf("%q is not a routed credential's name", n)
			}
		}
	}
	if len(r.Preferred) > 1 && slices.Contains(r.Preferred, NoCredentials) {
		return errors.New("credentials_preferred names none beside other credentials")
	}
	if slices.Contains(r.Names, NoCredentials) {
		return errors.New("credentials cannot name none; use no_credentials")
	}
	if len(r.Needs) > MaxNeeds {
		return fmt.Errorf("needs names more than %d hosts", MaxNeeds)
	}
	for _, h := range r.Needs {
		if !credproxy.ValidRouteHost(h) {
			return fmt.Errorf("needs: %q is not an exact host name", h)
		}
	}
	return nil
}

// heldRoutes opens a user's routed credentials and the organisation's,
// as candidates. A source that cannot be opened is an error, never a
// miss.
//
// @decision(DL-075)
func (s *Service) heldRoutes(ctx context.Context, orgID, userID string) ([]candidateRoute, []string, error) {
	var personal, org []HeldRoute
	if src := s.deps.RoutedCredentials; src != nil {
		held, err := src.Open(ctx, orgID, userID)
		if err != nil {
			return nil, nil, err
		}
		personal = held
	}
	if src := s.deps.OrgRoutedCredentials; src != nil {
		held, err := src.Open(ctx, orgID)
		if err != nil {
			return nil, nil, err
		}
		org = held
	}
	cands, notes := candidates(personal, org)
	return cands, notes, nil
}

// routesFor resolves the routed credentials a launch attaches, with the
// notes its progress carries.
//
// @decision(DL-075)
func (s *Service) routesFor(ctx context.Context, sess store.Session) ([]candidateRoute, []string, error) {
	cands, notes, err := s.heldRoutes(ctx, sess.OrgID, sess.OwnerID)
	if err != nil {
		return nil, nil, err
	}
	attached, more, err := attachRoutes(cands, sess.Request.Routed)
	if err != nil {
		return nil, nil, err
	}
	notes = append(notes, more...)
	for _, h := range uncoveredNeeds(attached, sess.Request.Routed.Needs) {
		notes = append(notes, fmt.Sprintf("the project needs %s, and no routed credential this session holds covers it", h))
	}
	return attached, notes, nil
}

// refuseRoutedNames refuses, at create, a launch list naming a routed
// credential the caller does not hold, so a typo is a refused request
// rather than a failed launch.
//
// @decision(DL-075)
func (s *Service) refuseRoutedNames(ctx context.Context, orgID, userID string, r store.RoutedRequest) error {
	if err := checkRoutedRequest(r); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if len(r.Names) == 0 {
		return nil
	}
	cands, _, err := s.heldRoutes(ctx, orgID, userID)
	if err != nil {
		return err
	}
	if _, _, err := attachRoutes(cands, r); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return nil
}

// ErrNoProxyForRoutes refuses a launch that would attach routed
// credentials on a server without the credential proxy.
//
// @decision(DL-077)
var ErrNoProxyForRoutes = errors.New("this server has no credential proxy, so it cannot hold routed credentials")

// refuseRoutesWithoutProxy refuses, at create, a launch that would
// attach any routed credential on a server without the proxy: the
// secret has no place but the main container, where it is never put.
//
// @decision(DL-077)
func (s *Service) refuseRoutesWithoutProxy(ctx context.Context, orgID, userID string, r store.RoutedRequest) error {
	if s.deps.ProxyImage != nil {
		return nil
	}
	cands, _, err := s.heldRoutes(ctx, orgID, userID)
	if err != nil || len(cands) == 0 {
		return err
	}
	attached, _, err := attachRoutes(cands, r)
	if err != nil || len(attached) == 0 {
		return err
	}
	return fmt.Errorf("%w: %w (%s); launch with --no-credentials, or build the server with scripts/build-proxy.sh",
		ErrInvalidRequest, ErrNoProxyForRoutes, routedNames(attached))
}

// recordedRoutes resolves a session's recorded routed credentials again,
// for a recreated sidecar: each in its recorded tier and with its
// recorded route. One removed since, or whose route has changed, is left
// out with a note, never replaced by another of the same name.
//
// @decision(DL-075)
func (s *Service) recordedRoutes(ctx context.Context, sess store.Session) ([]credproxy.Route, []string, error) {
	if len(sess.RoutedCredentials) == 0 {
		return nil, nil, nil
	}
	var personal, org []HeldRoute
	var err error
	if src := s.deps.RoutedCredentials; src != nil {
		if personal, err = src.Open(ctx, sess.OrgID, sess.OwnerID); err != nil {
			return nil, nil, err
		}
	}
	if src := s.deps.OrgRoutedCredentials; src != nil {
		if org, err = src.Open(ctx, sess.OrgID); err != nil {
			return nil, nil, err
		}
	}
	var out []credproxy.Route
	var notes []string
	for _, rec := range sess.RoutedCredentials {
		held := personal
		if Tier(rec.Tier) == TierOrg {
			held = org
		}
		i := slices.IndexFunc(held, func(h HeldRoute) bool { return h.Name == rec.Name })
		switch {
		case i < 0:
			notes = append(notes, fmt.Sprintf("routed credential %s was removed since the launch, so the new proxy does not hold it", rec.Name))
		case !sameRoute(held[i], rec):
			notes = append(notes, fmt.Sprintf("routed credential %s has a new route since the launch, so the new proxy does not hold it", rec.Name))
		default:
			out = append(out, held[i].Proxied())
		}
	}
	return out, notes, nil
}

// sameRoute reports whether a held credential still routes as the
// session recorded it.
func sameRoute(h HeldRoute, rec store.SessionRoutedCredential) bool {
	was := routedcred.Route{Name: rec.Name, Hosts: rec.Hosts, Paths: rec.Paths, Scheme: rec.Scheme,
		User: rec.User, Header: rec.Header, Preset: rec.Preset, Attach: h.Attach}
	return bytes.Equal(was.Encode(), h.Encode())
}

// MarkerRoutedLeftOut records, in a session's log, that a recreated
// sidecar left out a routed credential the launch had attached.
//
// @decision(DL-075)
const MarkerRoutedLeftOut = store.SpovNamespace + "session.routed_credential.left_out"

// noteRoutedLeftOut writes MarkerRoutedLeftOut; a failure to write only
// warns.
func (s *Service) noteRoutedLeftOut(ctx context.Context, sess store.Session, text string) {
	now := s.deps.Now()
	if _, err := s.deps.Store.Conversations().PutEvents(ctx, []store.Event{{
		ID: store.NewID(now), OrgID: sess.OrgID, SessionID: sess.ID, Source: store.SourceSpov,
		Name: MarkerRoutedLeftOut, Text: strings.ToUpper(text[:1]) + text[1:] + ".", At: now,
	}}); err != nil {
		s.deps.Warn(fmt.Sprintf("session %s: recording a left-out routed credential: %v", sess.ID, err))
	}
}

// routedNames lists names for a note.
func routedNames(cs []candidateRoute) string {
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.Name + " (" + strings.Join(c.Hosts, ", ") + ")"
	}
	return strings.Join(names, ", ")
}
