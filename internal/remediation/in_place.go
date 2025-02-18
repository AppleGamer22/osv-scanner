package remediation

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"deps.dev/util/resolve"
	"deps.dev/util/resolve/dep"
	"deps.dev/util/semver"
	"github.com/google/osv-scanner/internal/resolution"
	"github.com/google/osv-scanner/internal/resolution/client"
	lf "github.com/google/osv-scanner/internal/resolution/lockfile"
	"github.com/google/osv-scanner/internal/resolution/util"
	"github.com/google/osv-scanner/internal/utility/vulns"
	"golang.org/x/exp/maps"
)

type InPlacePatch struct {
	lf.DependencyPatch
	ResolvedVulns []resolution.ResolutionVuln
}

type InPlaceResult struct {
	Patches   []InPlacePatch
	Unfixable []resolution.ResolutionVuln
}

// ComputeInPlacePatches finds all possible targeting version changes that would fix vulnerabilities in a resolved graph.
// TODO: Check for introduced vulnerabilities
func ComputeInPlacePatches(ctx context.Context, cl client.ResolutionClient, graph *resolve.Graph, opts RemediationOptions) (InPlaceResult, error) {
	res, err := inPlaceVulnsNodes(cl, graph)
	if err != nil {
		return InPlaceResult{}, err
	}

	// Compute the overall constraints imposed by the dependent packages on the vulnerable nodes
	vkDependentConstraint := make(map[resolve.VersionKey]semver.Set)
	for vk, vulns := range res.vkVulns {
		reqVers := make(map[string]struct{})
		for _, vuln := range vulns {
			for _, c := range vuln.ProblemChains {
				_, req := c.EndDependency()
				reqVers[req] = struct{}{}
			}
		}
		set, err := buildConstraintSet(vk.Semver(), maps.Keys(reqVers))
		if err != nil {
			// TODO: log error?
			continue
		}
		vkDependentConstraint[vk] = set
	}

	var result InPlaceResult
	// TODO: This could be parallelized
	for vk, vulnList := range res.vkVulns {
		for _, vuln := range vulnList {
			if !opts.MatchVuln(vuln) {
				continue
			}
			// Consider vulns affecting packages we don't want to change unfixable
			if slices.Contains(opts.AvoidPkgs, vk.Name) {
				result.Unfixable = append(result.Unfixable, vuln)
				continue
			}
			newVK, err := findFixedVersion(ctx, cl, vk.PackageKey, func(newVK resolve.VersionKey) bool {
				// Check if this is a disallowed major version bump
				if !opts.AllowMajor {
					_, diff, err := vk.Semver().Difference(vk.Version, newVK.Version)
					if err != nil || diff == semver.DiffMajor {
						return false
					}
				}
				// Check if dependent packages are still satisfied by new version
				ok, err := vkDependentConstraint[vk].Match(newVK.Version)
				if err != nil || !ok {
					return false
				}

				// Check if new version's dependencies are satisfied by existing packages
				for _, nID := range res.vkNodes[vk] {
					ok, err := dependenciesSatisfied(ctx, cl, newVK, res.nodeDependencies[nID])
					if err != nil || !ok {
						return false
					}
				}

				// Check if this version is vulnerable
				return !vulns.IsAffected(vuln.Vulnerability, util.VKToPackageDetails(newVK))
			})

			if errors.Is(err, errInPlaceImpossible) {
				result.Unfixable = append(result.Unfixable, vuln)
				continue
			} else if err != nil {
				return InPlaceResult{}, err
			}

			dp := lf.DependencyPatch{
				Pkg:         vk.PackageKey,
				OrigVersion: vk.Version,
				NewVersion:  newVK.Version,
			}
			idx := slices.IndexFunc(result.Patches, func(ipp InPlacePatch) bool { return ipp.DependencyPatch == dp })
			if idx >= 0 {
				result.Patches[idx].ResolvedVulns = append(result.Patches[idx].ResolvedVulns, vuln)
			} else {
				result.Patches = append(result.Patches, InPlacePatch{
					DependencyPatch: dp,
					ResolvedVulns:   []resolution.ResolutionVuln{vuln},
				})
			}
		}
	}

	// Sort patches for priority/consistency
	slices.SortFunc(result.Patches, func(a, b InPlacePatch) int {
		// Number of vulns fixed descending
		if c := cmp.Compare(len(a.ResolvedVulns), len(b.ResolvedVulns)); c != 0 {
			return -c
		}
		// Package name ascending
		if c := cmp.Compare(a.Pkg.Name, b.Pkg.Name); c != 0 {
			return c
		}
		// Original version ascending
		if c := cmp.Compare(a.OrigVersion, b.OrigVersion); c != 0 {
			return c
		}
		// New version descending
		return -cmp.Compare(a.NewVersion, b.NewVersion)
	})

	return result, nil
}

var errInPlaceImpossible = errors.New("cannot find a version satisfying in-place constraints")

func findFixedVersion(ctx context.Context, cl client.DependencyClient, pk resolve.PackageKey, satifyFn func(resolve.VersionKey) bool) (resolve.VersionKey, error) {
	vers, err := cl.Versions(ctx, pk)
	if err != nil {
		return resolve.VersionKey{}, err
	}

	// Make sure versions are sorted, then iterate over versions from latest to earliest looking for a satisfying version
	slices.SortFunc(vers, func(a, b resolve.Version) int { return a.Semver().Compare(a.Version, b.Version) })
	for i := len(vers) - 1; i >= 0; i-- {
		vk := vers[i].VersionKey
		if vk.VersionType == resolve.Concrete && satifyFn(vk) {
			return vk, nil
		}
	}

	return resolve.VersionKey{}, errInPlaceImpossible
}

type inPlaceVulnsNodesResult struct {
	nodeDependencies map[resolve.NodeID][]resolve.VersionKey
	vkVulns          map[resolve.VersionKey][]resolution.ResolutionVuln
	vkNodes          map[resolve.VersionKey][]resolve.NodeID
}

func inPlaceVulnsNodes(cl client.VulnerabilityClient, graph *resolve.Graph) (inPlaceVulnsNodesResult, error) {
	nodeVulns, err := cl.FindVulns(graph)
	if err != nil {
		return inPlaceVulnsNodesResult{}, err
	}

	result := inPlaceVulnsNodesResult{
		nodeDependencies: make(map[resolve.NodeID][]resolve.VersionKey),
		vkVulns:          make(map[resolve.VersionKey][]resolution.ResolutionVuln),
		vkNodes:          make(map[resolve.VersionKey][]resolve.NodeID),
	}

	// Find all direct dependencies of vulnerable nodes.
	for _, e := range graph.Edges {
		if len(nodeVulns[e.From]) > 0 {
			result.nodeDependencies[e.From] = append(result.nodeDependencies[e.From], graph.Nodes[e.To].Version)
		}
	}

	// Construct ResolutionVulns for all vulnerable packages
	// combining nodes with the same package & versions number
	var nodeIDs []resolve.NodeID
	for nID, vulns := range nodeVulns {
		if len(vulns) > 0 {
			nodeIDs = append(nodeIDs, resolve.NodeID(nID))
		}
	}
	nodeChains := resolution.ComputeChains(graph, nodeIDs)
	// Computing ALL chains might be overkill...
	// We only actually care about the shortest chain, the unique dependents of the vulnerable node, and maybe the unique direct dependencies.

	for i, nID := range nodeIDs {
		chains := nodeChains[i]
		vk := graph.Nodes[nID].Version
		result.vkNodes[vk] = append(result.vkNodes[vk], nID)
		for _, vuln := range nodeVulns[nID] {
			resVuln := resolution.ResolutionVuln{
				Vulnerability: vuln,
				ProblemChains: slices.Clone(chains),
				DevOnly:       false, // TODO: parse & compute from lockfile
			}
			idx := slices.IndexFunc(result.vkVulns[vk], func(rv resolution.ResolutionVuln) bool { return rv.Vulnerability.ID == resVuln.Vulnerability.ID })
			if idx >= 0 {
				result.vkVulns[vk][idx].ProblemChains = append(result.vkVulns[vk][idx].ProblemChains, resVuln.ProblemChains...)
				result.vkVulns[vk][idx].DevOnly = result.vkVulns[vk][idx].DevOnly && resVuln.DevOnly
			} else {
				result.vkVulns[vk] = append(result.vkVulns[vk], resVuln)
			}
		}
	}

	return result, nil
}

func buildConstraintSet(sys semver.System, requiredVers []string) (semver.Set, error) {
	// combine a list of requirement strings into one semver.Set to allow version matching
	v := requiredVers[0]
	// 'latest' is effectively meaningless in a lockfile, since what 'latest' is could have changed between locking
	// TODO: other tags e.g. "next", "old" (?)
	// TODO: non-npm ecosystems
	if v == "latest" {
		v = "*"
	}
	c, err := sys.ParseConstraint(v)
	if err != nil {
		return semver.Set{}, err
	}
	cSet := c.Set()
	for _, req := range requiredVers[1:] {
		if req == "latest" {
			req = "*"
		}
		c, err := sys.ParseConstraint(req)
		if err != nil {
			return semver.Set{}, err
		}
		if err := cSet.Intersect(c.Set()); err != nil {
			return semver.Set{}, err
		}
	}

	return cSet, nil
}

func dependenciesSatisfied(ctx context.Context, cl client.DependencyClient, vk resolve.VersionKey, children []resolve.VersionKey) (bool, error) {
	var deps []resolve.VersionKey
	var optDeps []resolve.VersionKey
	reqs, err := cl.Requirements(ctx, vk)
	if err != nil {
		return false, err
	}

	for _, v := range reqs {
		if v.Type.IsRegular() {
			deps = append(deps, v.VersionKey)
		} else if v.Type.HasAttr(dep.Opt) {
			optDeps = append(optDeps, v.VersionKey)
		}
	}
	// TODO: correctly handle other attrs e.g. npm peerDependencies

	// remove the optional deps from the regular deps (because they show up in both) if they're not already installed
	for _, optVk := range optDeps {
		if !slices.ContainsFunc(children, func(vk resolve.VersionKey) bool { return vk.Name == optVk.Name }) {
			idx := slices.IndexFunc(deps, func(vk resolve.VersionKey) bool { return vk.Name == optVk.Name })
			deps = slices.Delete(deps, idx, idx+1)
		}
	}

	for _, depVK := range deps {
		ver := depVK.Version
		// 'latest' is effectively meaningless in a lockfile, since what 'latest' is could have changed between locking
		// TODO: Support other tags e.g. "next", "old" & non-npm ecosystems
		if ver == "latest" {
			ver = "*"
		}
		constr, err := vk.Semver().ParseConstraint(ver)
		if err != nil {
			return false, err
		}

		// check if any of the current children satisfy this import
		ok := false
		for _, child := range children {
			if child.Name == depVK.Name && constr.Match(child.Version) {
				ok = true
				break
			}
		}
		if !ok {
			return false, nil
		}
	}

	return true, nil
}
