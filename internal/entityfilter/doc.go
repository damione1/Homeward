// Package entityfilter admits a bounded AIP-160 Entity filter, then evaluates
// it solely against an immutable, already authorized materialized image.
// Neither parsing, candidate construction nor matching grants any access.
//
// Profile (https://google.aip.dev/160 and its linked EBNF):
//   - name and display_name are the only field selectors. Strings compare in
//     exact UTF-8 lexical order, without case folding or normalization.
//   - = and != support * only at literal boundaries. : is substring matching;
//     boundary wildcards select prefix/suffix/substring, and :* tests a
//     non-default value. An empty display_name equals "" but fails :*.
//   - Bare literals search name/display_name only, using substring semantics.
//   - device("homes/.../devices/..."), trait("power"), and
//     availability(AVAILABLE|UNAVAILABLE|UNKNOWN) are boolean functions. They
//     represent related data, not undeclared fields on the Entity resource.
//     Trait arguments use exact public Capability oneof field names.
//   - availability is existential across authorized observation qualities.
//     It never infers reachability from OFF/zero, freshness or provenance.
//     Missing quality is not an explicit UNKNOWN. NOT applies to the resulting
//     boolean, including when no observation exists.
//   - OR binds more tightly than explicit/implicit AND. NOT and - negate a
//     single simple expression. Parentheses provide explicit grouping.
//   - Unknown fields/functions/traits/enums, traversal and indices are errors.
//     Functions accept one typed literal; they cannot be compared or ordered.
//     Comparisons likewise accept one literal; group whole predicates instead
//     of using a logical expression as a comparison's right-hand value.
//
// Quoted strings support single/double quotes and JSON-style escapes plus
// escaped single quotes. Unquoted literals cannot contain traversal dots.
// Limits are measured before evaluation: 4096 input bytes, 256 tokens,
// 64 admitted nodes and eight nested groups/negations. No regex or SQL executes.
package entityfilter
