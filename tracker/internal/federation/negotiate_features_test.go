package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNegotiateFeatures_Intersection(t *testing.T) {
	mine := []string{"a", "b", "c"}
	theirs := []string{"b", "c", "d"}
	require.Equal(t, []string{"b", "c"}, NegotiateFeatures(mine, theirs))
}

func TestNegotiateFeatures_EmptyOnNoOverlap(t *testing.T) {
	require.Empty(t, NegotiateFeatures([]string{"a"}, []string{"b"}))
}

func TestNegotiateFeatures_PreservesMineOrder(t *testing.T) {
	require.Equal(t, []string{"z", "a"}, NegotiateFeatures([]string{"z", "a"}, []string{"a", "z"}))
}

func TestNegotiateFeatures_DedupesDuplicates(t *testing.T) {
	require.Equal(t, []string{"a"}, NegotiateFeatures([]string{"a", "a"}, []string{"a"}))
}
