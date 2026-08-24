package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/credential"
	"github.com/rhobuild/runpool/internal/platform/githubactions"
	"github.com/rhobuild/runpool/internal/store"

	"github.com/rhobuild/runpool/internal/assignment"
)

// buildBindings resolves every configured (target, tier) pair into a
// binding registered with the allocator. One GitHub client per target;
// tokens are resolved once.
//
// It reaches no provider. Startup reconciliation needs every binding in
// hand to adopt a running capsule or resolve an interrupted lease, so a
// binding must exist before the first provider call rather than as its
// result. Creating-or-adopting the scale set is the binding's own loop's
// first act, where a failure is retried instead of ending the process.
func (s *Controller) buildBindings(ctx context.Context, cfg *config.Config, environ func(string) string) error {
	var claimed []assignment.BindingID
	tiers := make(map[string]config.Tier, len(cfg.Tiers))
	for _, t := range cfg.Tiers {
		tiers[t.ID] = t
	}
	creds := make(map[string]config.Credential, len(cfg.Credentials))
	for _, c := range cfg.Credentials {
		creds[c.ID] = c
	}

	for _, target := range cfg.Targets {
		ref, err := config.ParseTargetURL(target.URL)
		if err != nil {
			return err
		}
		secret, err := credential.Resolve(environ, creds[target.CredentialID])
		if err != nil {
			return err
		}
		gh, err := githubactions.NewClient(githubactions.ClientConfig{
			ConfigURL:  ref.CanonicalURL,
			Credential: secret,
			Version:    cfg.Instance.Name,
		})
		if err != nil {
			return err
		}
		// Where this target's credential travels is stated at startup,
		// once per target, because nothing else makes it visible: any
		// https host is accepted by design, so a typo squatting one
		// letter away would otherwise authenticate quietly forever. A
		// host GitHub operates is ordinary; any other is the operator's
		// own claim, and the log says so at Warn without blocking it.
		if githubactions.IsHostedDomain(ref.Host) {
			s.log.Info("target authenticates against the provider",
				"target", target.ID, "host", ref.Host, "credential", target.CredentialID)
		} else {
			s.log.Warn("target authenticates against a host GitHub does not operate; "+
				"the credential travels there on every call",
				"target", target.ID, "host", ref.Host, "credential", target.CredentialID)
		}

		for _, tb := range target.Tiers {
			tier := tiers[tb.TierID]

			// The neutral binding row comes first: it keys the delivery,
			// attempt and lease machinery.
			sourceBindingKey := sourceBindingKey(target, tb.ScaleSetName)
			var bindingID assignment.BindingID
			var knownSetID int64
			if err := s.store.Tx(ctx, func(tx *store.Tx) error {
				var err error
				if bindingID, err = tx.EnsureBinding(assignment.TargetID(target.ID), "github_actions", sourceBindingKey); err != nil {
					return err
				}
				// The scale set id recorded against this binding is the
				// proof of ownership that lets an existing set be adopted.
				// Without it, a set that merely shares the name is a
				// stranger's.
				knownSetID, err = tx.GitHubScaleSetID(bindingID)
				return err
			}); err != nil {
				return err
			}
			b := &binding{
				key:    assignment.BindingKey(target.ID + "/" + tier.ID),
				target: target,
				tier:   tier,
				ref:    ref,
				gh:     gh,
				// The recorded id is what lets the loop adopt the set it
				// created on an earlier start instead of refusing a
				// stranger's set of the same name. Zero means this binding
				// has never had one.
				scaleSetID:   int(knownSetID),
				scaleSetName: tb.ScaleSetName,
				bindingID:    bindingID,
				// Only a repository-scoped binding may bind a persistent
				// cache: an organization runner is not bound to the job
				// whose demand created it, so it could execute any
				// repository's job against another repository's cache.
				cacheEnabled: ref.Scope == config.ScopeRepository && target.Cache.Enabled,
				generation:   target.Cache.Generation,
				maxLanes:     laneCeiling(cfg, tier),
				capsuleImage: tier.Image(s.shippedCapsuleImage),
			}
			if err := s.alloc.Register(assignment.TierID(tier.ID), b.key, tier.Parallelism); err != nil {
				return err
			}
			s.bindings = append(s.bindings, b)
			s.byBinding[bindingID] = b
			claimed = append(claimed, bindingID)
		}
	}
	if len(s.bindings) == 0 {
		return errors.New("no bindings configured")
	}
	// What configuration no longer claims is forgotten. A renamed scale
	// set or a removed tier leaves a row nothing serves, and the report
	// carries it forever with no command that removes it.
	//
	// A binding that still holds deliveries is kept: forgetting it would
	// orphan the attempts hanging off those rows. Everything else goes,
	// including the recorded scale set id — which is why a rename is a
	// migration rather than an edit, and why sourceBindingKey's own
	// documentation says what a moved key costs.
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		forgotten, err := tx.ForgetUnclaimedBindings(claimed)
		if err != nil {
			return err
		}
		if forgotten > 0 {
			s.log.Info("forgot bindings configuration no longer claims", "count", forgotten)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// bindingKeyVersion prefixes the binding key, and is not the delivery
// key's version even though it reads the same today.
//
// Bumping it renames every binding. The new key matches no row, so a row
// is written for it and the old one is unclaimed — ForgetUnclaimedBindings
// deletes it on the same startup, taking the scale set id recorded
// against it, unless it still holds deliveries. Every binding then meets
// a provider that already has its scale set under the unchanged name and
// refuses to adopt one it has no record of creating, once each, before
// settling. The contact history does not come back.
//
// That is a migration, not an encoding change. Anything that makes this
// value move deserves the same reading as renaming a targets[].id, which
// docs/adrs/2026-08-17-target-hosts-and-scopes.md records.
const bindingKeyVersion = "v2"

// sourceBindingKey is a binding's durable identity: the row every
// delivery, attempt and lease hangs off.
//
// It is built from what an operator configured and never from a parsed
// form of the target's URL. A key carrying the parsed scope and the
// canonical URL moves whenever the parser changes how it reads an
// address a deployment did not touch, and a key that moves is a rename:
// the new one matches no row, so a row is written for it, and the old
// one is forgotten by ForgetUnclaimedBindings on the same startup —
// taking the scale set id recorded against it. The next pass then has to
// adopt a set it has no record of creating, which is a refusal before it
// is an adoption. Scope and canonical URL still travel, in the adapter's
// own metadata, which is where provider identity belongs.
func sourceBindingKey(target config.Target, scaleSetName string) assignment.SourceBindingKey {
	return assignment.SourceBindingKey(fmt.Sprintf("%s|%s|%s|%s",
		bindingKeyVersion, target.ID, target.RunnerGroup, scaleSetName))
}

// ensureScaleSet creates or adopts this binding's scale set and records
// the provider's own id against it. It runs from the binding's loop, so a
// provider that is unreachable costs this binding its turn rather than
// costing the process its startup: the capsules other bindings adopted
// keep running, and their exits are still observed.
func (s *Controller) ensureScaleSet(ctx context.Context, b *binding) error {
	// Creating a set at the provider and writing its id here are two
	// steps, and the gap between them is where the process can die. The
	// name is therefore written down first, as an intention: a row with
	// no id says this binding asked for this name and does not yet know
	// what it got. Without it, a crash in that gap leaves a set the
	// provider has and this instance cannot account for, and every later
	// start refuses to adopt it — correctly, because a set that merely
	// shares a name is a stranger's. The intention is what tells the two
	// apart.
	//
	// It is recorded only once the provider says the name is free, which
	// is why the write travels as a callback instead of happening here. An
	// intention written before the lookup is left behind by a pass that is
	// then refused, and reads on the next pass as proof this instance
	// created the stranger it had just declined.
	var intended bool
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		_, recorded, err := tx.GitHubScaleSet(b.bindingID)
		intended = recorded
		return err
	}); err != nil {
		return err
	}
	recordIntent := func() error {
		return s.store.Tx(ctx, func(tx *store.Tx) error {
			return tx.RecordGitHubBindingMetadata(b.bindingID, string(b.ref.Scope),
				b.ref.CanonicalURL, b.target.RunnerGroup, b.scaleSetName, 0)
		})
	}

	set, err := b.gh.EnsureScaleSet(ctx, b.target.RunnerGroup, b.scaleSetName,
		b.scaleSetID, intended, recordIntent)
	if err != nil {
		return err
	}
	// The id is written on a context that outlives this one: the set now
	// exists, and a shutdown arriving here would otherwise discard the
	// only record of which one, leaving the next start to adopt it
	// through the intention above rather than by knowing its id.
	record, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := s.store.Tx(record, func(tx *store.Tx) error {
		return tx.RecordGitHubBindingMetadata(b.bindingID, string(b.ref.Scope),
			b.ref.CanonicalURL, b.target.RunnerGroup, b.scaleSetName, int64(set.ID))
	}); err != nil {
		return err
	}
	b.scaleSetID = set.ID
	b.ensured = true
	s.log.Info("scale set ready", "binding", b.key, "name", b.scaleSetName,
		"id", set.ID, "adopted", set.Adopted, "scope", string(b.ref.Scope))
	return nil
}

// laneCeiling is the real concurrency ceiling for one tier: its own
// parallelism, capped by the instance-wide limit when that is tighter.
// Sizing cache lanes off the tier alone would provision a lane for every
// lease the tier could theoretically hold, which a global limit forbids.
func laneCeiling(cfg *config.Config, tier config.Tier) int {
	if cfg.Scheduling.Parallelism == nil {
		return tier.Parallelism
	}
	return min(tier.Parallelism, *cfg.Scheduling.Parallelism)
}

// contactHeartbeat is the longest a recorded reach may go unrefreshed
// while nothing about it changes. What the record answers is "how long
// has this been so", so a state that persists is written on a heartbeat
// rather than on every observation of it — the loop's own pace is the
// broker's long poll, but a broker that answers at once would otherwise
// turn a reporting field into a write per iteration, on a database whose
// single connection every lease transition also needs.
const contactHeartbeat = 15 * time.Second

// recordProviderContact marks this binding as reaching its provider.
//
// Failing to record it is not worth interrupting serving for: the record
// exists so an operator can see a binding that reaches nothing, and a
// binding that cannot write is one whose store is already reporting for
// itself.
func (s *Controller) recordProviderContact(ctx context.Context, b *binding) {
	// Cancelled on the way out of a shutdown, where a store write would
	// fail for that reason alone and say nothing about the provider.
	if ctx.Err() != nil {
		return
	}
	now := time.Now()
	// A transition is always written; a state that has not changed waits
	// out the heartbeat. Recovering from a failure is a transition, which
	// is why the failure path clears the pacing rather than leaving a
	// recovery to wait.
	if b.reaching && now.Sub(b.lastContactWrite) < contactHeartbeat {
		return
	}
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		return tx.RecordProviderContact(b.bindingID, now)
	}); err != nil {
		s.log.Warn("cannot record provider contact", "binding", b.key, "error", err)
		return
	}
	b.lastContactWrite = now
	b.reaching = true
}

// recordProviderFailure records what this binding currently cannot do
// with its provider.
//
// A failure is written whenever it is new or different, and otherwise on
// the same heartbeat as a success: what an operator needs is what is
// wrong and how long it has been wrong, and neither answer improves by
// rewriting the same sentence every five seconds.
func (s *Controller) recordProviderFailure(ctx context.Context, b *binding, cause error) {
	if ctx.Err() != nil {
		return
	}
	now, reason := time.Now(), cause.Error()
	if b.reaching || reason != b.lastFailure || now.Sub(b.lastContactWrite) >= contactHeartbeat {
		if err := s.store.Tx(ctx, func(tx *store.Tx) error {
			return tx.RecordProviderFailure(b.bindingID, now, reason)
		}); err != nil {
			s.log.Warn("cannot record provider failure", "binding", b.key, "error", err)
			return
		}
		b.lastContactWrite = now
	}
	b.reaching = false
	b.lastFailure = reason
}

// firstOf keeps the first moment of a run of like events: the second and
// later ones are the same episode, and its age is what decides whether it
// is still ordinary.
func firstOf(existing, now time.Time) time.Time {
	if existing.IsZero() {
		return now
	}
	return existing
}
