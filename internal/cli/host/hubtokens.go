package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/termsafe"
)

// uatPrefix starts every hub access token, and the part of it the hub lists
// as a token's prefix (scion's store.UATPrefix); the prefix goes on with the
// first characters of the secret.
const uatPrefix = "scion_pat_"

// leverTokens is what makes a controller token lever's: an id lever recorded
// (the controller and remote PAT records, the `lever ssh` tokens), or a
// secret lever holds, which the hub's prefix of the token matches.
type leverTokens struct {
	ids        map[string]bool
	controller string
	secrets    []string
}

// loadLeverTokens fails on any record or secret it cannot read: lever never
// calls a token unrecorded without its own records in hand.
func loadLeverTokens(st state.State) (leverTokens, error) {
	k := leverTokens{ids: map[string]bool{}}
	for _, load := range []func() (state.PATRecord, bool, error){st.LoadControllerPATRecord, st.LoadRemotePATRecord} {
		r, found, err := load()
		if err != nil {
			return k, err
		}
		if found && r.ID != "" {
			k.ids[r.ID] = true
		}
	}
	ssh, err := st.SSHTokens()
	if err != nil {
		return k, err
	}
	for _, id := range ssh {
		k.ids[id] = true
	}
	if k.controller, err = st.LoadControllerPAT(); err != nil {
		return k, err
	}
	remote, err := st.LoadRemotePAT()
	if err != nil {
		return k, err
	}
	for _, s := range []string{k.controller, remote} {
		if s != "" {
			k.secrets = append(k.secrets, s)
		}
	}
	return k, nil
}

// prefixUsable: a prefix that is missing or names no more than the token
// type cannot tell one token from another.
func prefixUsable(p string) bool {
	return len(p) > len(uatPrefix) && strings.HasPrefix(p, uatPrefix)
}

func (k leverTokens) holds(t hubapi.Token) bool {
	if !prefixUsable(t.Prefix) {
		return false
	}
	for _, s := range k.secrets {
		if strings.HasPrefix(s, t.Prefix) {
			return true
		}
	}
	return false
}

// unrecorded sorts the controller's live tokens that are not lever's:
// revoke are provably neither recorded nor held, unknown are unrecorded but
// carry no prefix that could show lever does not hold them. A `lever ssh`
// token is recorded right after its mint, so an unrecorded one that has not
// expired may belong to a session starting now; it is left alone.
func (k leverTokens) unrecorded(toks []hubapi.Token, now time.Time) (revoke, unknown []hubapi.Token) {
	for _, t := range toks {
		switch {
		case t.Revoked, k.ids[t.ID], k.holds(t):
		case t.Name == sshTokenName && (t.ExpiresAt.IsZero() || t.ExpiresAt.After(now)):
		case !prefixUsable(t.Prefix):
			unknown = append(unknown, t)
		default:
			revoke = append(revoke, t)
		}
	}
	return revoke, unknown
}

// controllerListed reports whether toks holds a live token with the
// controller PAT's prefix: the list is the controller's own, and the hub's
// prefixes name lever's secrets.
func (k leverTokens) controllerListed(toks []hubapi.Token) bool {
	if k.controller == "" {
		return false
	}
	for _, t := range toks {
		if !t.Revoked && prefixUsable(t.Prefix) && strings.HasPrefix(k.controller, t.Prefix) {
			return true
		}
	}
	return false
}

func tokenLabel(t hubapi.Token) string {
	return termsafe.Sanitize(t.ID) + " (" + termsafe.Sanitize(t.Name) + ")"
}

// revokeUnrecordedTokens revokes the controller's tokens lever neither
// recorded nor holds: PATs older levers minted before they kept records,
// which keep the controller's authority until revoked, and which only lever
// can revoke (a token cannot manage tokens). It runs after the mint, so the
// records and secrets on disk are the current ones. Anything that leaves the
// list in doubt revokes nothing: a record or secret lever cannot read, a
// controller assertion that does not resolve to the recorded controller, or
// a list without the controller PAT lever holds. Failures only warn; the next
// apply tries again.
func (h *hubIdentity) revokeUnrecordedTokens(ctx context.Context, mode, controllerID string) {
	const skip = "hub-identity: the controller's unrecorded hub tokens are not revoked this time: "
	keep, err := loadLeverTokens(h.st)
	if err != nil {
		h.warn(skip+"lever cannot read its own token records: %v", err)
		return
	}
	if keep.controller == "" {
		h.warn(skip + "lever holds no controller token")
		return
	}
	me, err := h.assertController(ctx, mode)
	if err != nil {
		h.warn(skip+"%v", err)
		return
	}
	if me.ID != controllerID {
		h.warn(skip+"the controller assertion resolved to %s, not the recorded controller %s", termsafe.Sanitize(me.ID), controllerID)
		return
	}
	toks, err := h.ctl.ListTokens(ctx)
	if err != nil {
		h.warn(skip+"listing them failed: %v", err)
		return
	}
	if !keep.controllerListed(toks) {
		h.warn(skip + "the hub's list does not show the controller token lever holds, so lever cannot tell its own tokens apart")
		return
	}
	revoke, unknown := keep.unrecorded(toks, h.guestNow())
	for _, t := range unknown {
		h.warn("hub-identity: the controller holds token %s that lever did not record, and the hub lists no prefix that shows whether lever uses it; it is not revoked", tokenLabel(t))
	}
	for _, t := range revoke {
		switch err := h.ctl.RevokeToken(ctx, t.ID); {
		case err == nil, errors.Is(err, hubapi.ErrNotFound):
			h.log("lever: revoked hub token %s of the controller, valid until %s: lever had no record of it", tokenLabel(t), expiryText(t.ExpiresAt))
		default:
			h.warn("hub-identity: could not revoke the controller's unrecorded hub token %s (it stays valid until %s; the next apply tries again): %v",
				tokenLabel(t), expiryText(t.ExpiresAt), err)
		}
	}
}

// unrecordedTokenProblem is doctor's view of the same list: "" when the
// controller holds only lever's tokens.
func unrecordedTokenProblem(keep leverTokens, toks []hubapi.Token, now time.Time) string {
	revoke, unknown := keep.unrecorded(toks, now)
	var parts []string
	if len(revoke) > 0 {
		parts = append(parts, "the controller holds tokens lever did not record: "+tokenLabels(revoke))
	}
	if len(unknown) > 0 {
		parts = append(parts, fmt.Sprintf("the controller holds tokens lever did not record and cannot tell from its own (the hub lists no prefix): %s", tokenLabels(unknown)))
	}
	return strings.Join(parts, "; ")
}

func tokenLabels(toks []hubapi.Token) string {
	labels := make([]string, 0, len(toks))
	for _, t := range toks {
		labels = append(labels, tokenLabel(t))
	}
	slices.Sort(labels)
	return strings.Join(labels, ", ")
}
