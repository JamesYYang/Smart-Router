package budget

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestIsMongoTransactionCapabilityError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "standalone transaction message",
			err:  errors.New("Transaction numbers are only allowed on a replica set member or mongos"),
			want: true,
		},
		{
			name: "illegal operation command code",
			err: mongo.CommandError{
				Code:    20,
				Message: "transaction is not supported by this deployment",
				Labels:  []string{"TransientTransactionError"},
			},
			want: true,
		},
		{
			name: "labeled unsupported transaction message",
			err: mongo.CommandError{
				Message: "transaction is not supported by this deployment",
				Labels:  []string{"TransientTransactionError"},
			},
			want: true,
		},
		{
			name: "ordinary transient transaction error",
			err: mongo.CommandError{
				Message: "temporary write conflict",
				Labels:  []string{"TransientTransactionError"},
			},
			want: false,
		},
		{
			name: "ordinary error",
			err:  errors.New("network timeout"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMongoTransactionCapabilityError(tt.err); got != tt.want {
				t.Fatalf("isMongoTransactionCapabilityError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestMongoUsagePathMatchIncludesLegacyRootRows(t *testing.T) {
	got := mongoUsagePathMatch("/")
	want := bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "user_path", Value: bson.D{{Key: "$exists", Value: false}}}},
		bson.D{{Key: "user_path", Value: bson.D{{Key: "$regex", Value: `^\s*$`}}}},
		bson.D{{Key: "user_path", Value: bson.D{{Key: "$regex", Value: "^/"}}}},
	}}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mongoUsagePathMatch(/) = %#v, want %#v", got, want)
	}
}

func TestMongoUsagePathMatchNestedPathRequiresPrefixBoundary(t *testing.T) {
	got := mongoUsagePathMatch("/team")
	want := bson.D{{Key: "user_path", Value: bson.D{{Key: "$regex", Value: `^/team(?:/|$)`}}}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mongoUsagePathMatch(/team) = %#v, want %#v", got, want)
	}
}

func TestMongoUncachedUsageMatchIncludesMissingNilAndEmptyCacheType(t *testing.T) {
	got := mongoUncachedUsageMatch()
	want := bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "cache_type", Value: bson.D{{Key: "$exists", Value: false}}}},
		bson.D{{Key: "cache_type", Value: nil}},
		bson.D{{Key: "cache_type", Value: ""}},
	}}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mongoUncachedUsageMatch() = %#v, want %#v", got, want)
	}
}

func TestMongoUsageCostMatchAggregateExcludesUserPath(t *testing.T) {
	start := time.Date(2026, time.April, 25, 11, 0, 0, 0, time.UTC)
	end := time.Date(2026, time.April, 25, 13, 0, 0, 0, time.UTC)

	got := mongoUsageCostMatch("/team", start, end, true)
	if mongoDocContainsKey(got, "user_path") {
		t.Fatalf("aggregate $match must not filter on user_path, got %#v", got)
	}
	and, ok := mongoMatchAndElement(got)
	if !ok {
		t.Fatalf("aggregate $match must keep the $and element, got %#v", got)
	}
	if !reflect.DeepEqual(and, bson.A{mongoUncachedUsageMatch()}) {
		t.Fatalf("aggregate $and = %#v, want only mongoUncachedUsageMatch()", and)
	}
}

func TestMongoUsageCostMatchPerPathKeepsUserPath(t *testing.T) {
	start := time.Date(2026, time.April, 25, 11, 0, 0, 0, time.UTC)
	end := time.Date(2026, time.April, 25, 13, 0, 0, 0, time.UTC)

	got := mongoUsageCostMatch("/team", start, end, false)
	and, ok := mongoMatchAndElement(got)
	if !ok {
		t.Fatalf("per-path $match must keep the $and element, got %#v", got)
	}
	if !reflect.DeepEqual(and, bson.A{mongoUsagePathMatch("/team"), mongoUncachedUsageMatch()}) {
		t.Fatalf("per-path $and = %#v, want user-path match + uncached match", and)
	}
}

// mongoMatchAndElement returns the $and element of a $match document.
func mongoMatchAndElement(doc bson.D) (bson.A, bool) {
	for _, el := range doc {
		if el.Key == "$and" {
			and, ok := el.Value.(bson.A)
			return and, ok
		}
	}
	return nil, false
}

// mongoDocContainsKey reports whether any key matching name appears anywhere
// in the document, including nested documents and arrays.
func mongoDocContainsKey(doc bson.D, name string) bool {
	for _, el := range doc {
		if el.Key == name {
			return true
		}
		switch v := el.Value.(type) {
		case bson.D:
			if mongoDocContainsKey(v, name) {
				return true
			}
		case bson.A:
			for _, item := range v {
				if nested, ok := item.(bson.D); ok && mongoDocContainsKey(nested, name) {
					return true
				}
			}
		}
	}
	return false
}
