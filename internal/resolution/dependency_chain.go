package resolution

import (
	"context"
	"slices"

	"deps.dev/util/resolve"
	"github.com/google/osv-scanner/internal/resolution/manifest"
	"github.com/google/osv-scanner/internal/resolution/util"
	vulnUtil "github.com/google/osv-scanner/internal/utility/vulns"
	"github.com/google/osv-scanner/pkg/lockfile"
	"github.com/google/osv-scanner/pkg/models"
)

type DependencyChain struct {
	Graph *resolve.Graph
	Edges []resolve.Edge // Edge from root node is at the end of the list
}

func (dc DependencyChain) DirectDependency() (resolve.VersionKey, string) {
	edge := dc.Edges[len(dc.Edges)-1]
	return dc.Graph.Nodes[edge.To].Version, edge.Requirement
}

func (dc DependencyChain) EndDependency() (resolve.VersionKey, string) {
	edge := dc.Edges[0]
	return dc.Graph.Nodes[edge.To].Version, edge.Requirement
}

func ChainIsDev(dc DependencyChain, m manifest.Manifest) bool {
	direct, _ := dc.DirectDependency()
	ecosystem, ok := util.OSVEcosystem[direct.System]
	if !ok {
		return false
	}

	return lockfile.Ecosystem(ecosystem).IsDevGroup(m.Groups[direct.PackageKey])
}

// ComputeChains computes all paths from each specified NodeID to the root node.
func ComputeChains(g *resolve.Graph, nodes []resolve.NodeID) [][]DependencyChain {
	// find the parent nodes of each node in graph, for easier traversal
	parentEdges := make(map[resolve.NodeID][]resolve.Edge)
	for _, e := range g.Edges {
		// check for a self-dependency, just in case
		if e.From == e.To {
			continue
		}
		parentEdges[e.To] = append(parentEdges[e.To], e)
	}

	allChains := make([][]DependencyChain, len(nodes))
	// for each node, traverse up all possible paths to the root node
	for i, node := range nodes {
		var toProcess []DependencyChain
		for _, pEdge := range parentEdges[node] {
			toProcess = append(toProcess, DependencyChain{
				Graph: g,
				Edges: []resolve.Edge{pEdge},
			})
		}
		for len(toProcess) > 0 {
			chain := toProcess[0]
			toProcess = toProcess[1:]
			edge := chain.Edges[len(chain.Edges)-1]
			if edge.From == 0 { // we are at the root, add it to the final list
				allChains[i] = append(allChains[i], chain)
				continue
			}
			// add all parent edges to the queue
			for _, pEdge := range parentEdges[edge.From] {
				// check for a dependency cycle before adding them
				if !slices.ContainsFunc(chain.Edges, func(e resolve.Edge) bool { return e.To == pEdge.To }) {
					toProcess = append(toProcess, DependencyChain{
						Graph: g,
						Edges: append(slices.Clone(chain.Edges), pEdge),
					})
				}
			}
		}
	}

	return allChains
}

// chainConstrains check if a DependencyChain is 'Problematic'
// i.e. if it is forcing the vulnerable package to chosen in resolution.
func chainConstrains(ctx context.Context, cl resolve.Client, chain DependencyChain, vuln *models.Vulnerability) bool {
	// TODO: Logic needs to be ecosystem-specific.
	if len(chain.Edges) == 0 {
		return false
	}
	// Just check if the direct requirement of the vulnerable package is constraining it.
	// This still has some false positives.
	// e.g. if we have
	// A@* -> B@2.*
	// D@* -> B@2.1.1 -> C@1.0.0
	// resolving both together picks B@2.1.1 & thus constrains C to C@1.0.0 for A
	// But resolving A alone could pick B@2.2.0 which might not depend on C
	// Similarly, a direct dependency could be constrained by an indirect dependency with similar results.

	// Check if the latest allowable version of the package is vulnerable
	vk, req := chain.EndDependency()
	vk.Version = req
	vk.VersionType = resolve.Requirement
	vers, err := cl.MatchingVersions(ctx, vk)
	if err != nil {
		// TODO: handle error
		return true
	}

	bestVk := vers[len(vers)-1] // This should be the highest version for npm

	return vulnUtil.IsAffected(*vuln, util.VKToPackageDetails(bestVk.VersionKey))
}
