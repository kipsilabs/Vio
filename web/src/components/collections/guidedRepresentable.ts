import {
  normalizeQueryDefinition,
  type QueryDefinition,
  type QueryDefinitionInput,
} from "@/api/types";

import {
  guidedStateToQueryDefinition,
  queryDefinitionToGuidedState,
} from "./CollectionGuidedRulesEditor";

export const GUIDED_UNAVAILABLE_MESSAGE = "These rules use options the Guided view can't show.";

/**
 * Reports whether the Guided editor can edit qd without losing anything. Guided
 * rebuilds the whole definition from its form state, so a definition qualifies
 * only when that rebuild reproduces it: no OR groups, negated rules, repeated
 * person rules or fields Guided has no control for. Rule and group order are
 * ignored because they don't change which items match.
 */
export function isGuidedRepresentable(qd: QueryDefinition | QueryDefinitionInput): boolean {
  const normalized = normalizeQueryDefinition(qd);
  const rebuilt = guidedStateToQueryDefinition(
    queryDefinitionToGuidedState(normalized),
    normalized,
  );
  return comparisonKey(rebuilt) === comparisonKey(normalized);
}

function comparisonKey(qd: QueryDefinition): string {
  const groups = qd.groups
    .map((group) => {
      const rules = group.rules
        .map(({ field, op, value }) =>
          // Genre is an array field, where the server runs "contains" and "is"
          // as the same query, so Guided's switch to "is" changes nothing.
          JSON.stringify([field, field === "genre" && op === "contains" ? "is" : op, value]),
        )
        .sort();
      return JSON.stringify([group.match, rules]);
    })
    .sort();
  return JSON.stringify({ ...qd, groups });
}
