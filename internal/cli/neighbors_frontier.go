package cli

import (
	"sort"

	"github.com/entireio/entire-graph/internal/sem"
)

// Counts describe relation records in this snapshot, not distinct reachable
// entities. A missing endpoint record is outside the renderable eligible set.
type neighborFrontierDirect struct {
	EligibleEdges        int `json:"eligible_edges"`
	EmittedEdges         int `json:"emitted_edges"`
	OmittedEdges         int `json:"omitted_edges"`
	MissingEndpointEdges int `json:"missing_endpoint_edges"`
}

type neighborFrontierCount struct {
	Relation   string  `json:"relation"`
	Resolution *string `json:"resolution"`
	Edges      int     `json:"edges"`
}

type neighborFrontierDegree struct {
	Counts               []neighborFrontierCount `json:"counts"`
	ExternalEdges        int                     `json:"external_edges"`
	MissingEndpointEdges int                     `json:"missing_endpoint_edges"`
	ReturnsToFocus       int                     `json:"returns_to_focus"`
	SelfEdges            int                     `json:"self_edges"`
}

type neighborFrontierEndpoint struct {
	Kind       string `json:"kind"`
	EndpointID string `json:"endpoint_id"`
	Reason     string `json:"reason,omitempty"`
	*neighborFrontierDegree
}

type neighborFrontierObservation struct {
	Direct   neighborFrontierDirect     `json:"direct"`
	Frontier []neighborFrontierEndpoint `json:"frontier"`
}

type neighborFrontierSummary struct {
	Mode              string   `json:"mode"`
	Kind              string   `json:"kind"`
	Selection         string   `json:"selection"`
	SelectedRelations []string `json:"selected_relations"`
	Direction         string   `json:"direction"`
	Depth             int      `json:"depth"`
	IndexedCoverage   string   `json:"indexed_coverage"`
	*neighborFrontierObservation
}

type neighborFrontierBin struct {
	relation   string
	resolution string
}

type neighborFrontierAggregate struct {
	degree *neighborFrontierDegree
	bins   map[neighborFrontierBin]int
}

func buildNeighborFrontier(snapshot sem.ProviderSnapshot, flags neighborFlags, endpoints map[string]neighborEndpoint, response neighborResponse) *neighborFrontierSummary {
	coverage := snapshot.Header.Completeness.Health.Status
	if coverage == "" {
		coverage = "unknown"
	}
	summary := &neighborFrontierSummary{
		Mode: "frontier", Kind: "unresolved_focus", Selection: response.Selection,
		SelectedRelations: response.SelectedRelations, Direction: flags.Direction,
		Depth: 1, IndexedCoverage: coverage,
	}
	if response.DisambiguationRequired || len(response.Matches) != 1 {
		return summary
	}
	focus := response.Matches[0]
	edges := focus.Outgoing
	if flags.Direction == "in" {
		edges = focus.Incoming
	}
	observation := &neighborFrontierObservation{
		Direct:   neighborFrontierDirect{EmittedEdges: len(edges)},
		Frontier: make([]neighborFrontierEndpoint, 0, len(edges)),
	}
	summary.Kind = "observed"
	summary.neighborFrontierObservation = observation
	retained := make(map[string]neighborEndpoint, len(edges))
	for _, edge := range edges {
		retained[edge.Endpoint.ID] = edge.Endpoint
	}
	ids := make([]string, 0, len(retained))
	for id := range retained {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	aggregates := make(map[string]neighborFrontierAggregate, len(ids))
	for _, id := range ids {
		endpoint := retained[id]
		entry := neighborFrontierEndpoint{Kind: "unavailable", EndpointID: id}
		switch {
		case endpoint.External:
			entry.Reason = "external_endpoint"
		case snapshot.Header.LanguageTiers[endpoint.Language] == "inventory-only":
			entry.Reason = "inventory_only_endpoint"
		case snapshot.Header.LanguageTiers[endpoint.Language] != "semantic":
			entry.Reason = "unclassified_endpoint"
		default:
			entry.Kind = "indexed"
			entry.neighborFrontierDegree = &neighborFrontierDegree{Counts: []neighborFrontierCount{}}
			aggregates[id] = neighborFrontierAggregate{degree: entry.neighborFrontierDegree, bins: make(map[neighborFrontierBin]int)}
		}
		observation.Frontier = append(observation.Frontier, entry)
	}
	selected := make(map[string]bool, len(response.SelectedRelations))
	for _, relation := range response.SelectedRelations {
		selected[relation] = true
	}
	for _, relation := range snapshot.Relations {
		if !selected[relation.Type] {
			continue
		}
		from, to := relation.FromID, relation.ToID
		if flags.Direction == "in" {
			from, to = to, from
		}
		aggregate, retained := aggregates[from]
		if from != focus.Symbol.ID && !retained {
			continue
		}
		endpoint, found := endpoints[to]
		if !found {
			if from == focus.Symbol.ID {
				observation.Direct.MissingEndpointEdges++
			}
			if retained {
				aggregate.degree.MissingEndpointEdges++
			}
			continue
		}
		if !neighborEndpointAllowed(endpoint, flags) {
			continue
		}
		if from == focus.Symbol.ID {
			observation.Direct.EligibleEdges++
		}
		if !retained {
			continue
		}
		aggregate.bins[neighborFrontierBin{relation.Type, relation.Resolution}]++
		if endpoint.External {
			aggregate.degree.ExternalEdges++
		}
		if to == focus.Symbol.ID {
			aggregate.degree.ReturnsToFocus++
		}
		if to == from {
			aggregate.degree.SelfEdges++
		}
	}
	observation.Direct.OmittedEdges = observation.Direct.EligibleEdges - observation.Direct.EmittedEdges
	for _, aggregate := range aggregates {
		for bin, count := range aggregate.bins {
			var resolution *string
			if bin.resolution != "" {
				value := bin.resolution
				resolution = &value
			}
			aggregate.degree.Counts = append(aggregate.degree.Counts, neighborFrontierCount{Relation: bin.relation, Resolution: resolution, Edges: count})
		}
		sort.Slice(aggregate.degree.Counts, func(i, j int) bool {
			left, right := aggregate.degree.Counts[i], aggregate.degree.Counts[j]
			if left.Relation != right.Relation {
				return left.Relation < right.Relation
			}
			if left.Resolution == nil {
				return right.Resolution != nil
			}
			return right.Resolution != nil && *left.Resolution < *right.Resolution
		})
	}
	return summary
}
