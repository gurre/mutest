// Package operators is a fixture holding one site for every mutation operator, so a test can ask
// the generator what it produces rather than restate the list and agree with itself.
//
// It lives under testdata, which the go tool never builds and mutation.Packages skips, so it is
// read only by go/parser.
package operators

import (
	"context"
	"errors"
	"log"
	"sort"
)

const prefix = "slot_"

const share = 0.5

// stored is a site for the struct-tag operator. Only the storage tag's omitempty is mutable; the
// json one beside it is there to show it is left alone.
type stored struct {
	UserID string `json:"userId,omitempty" dynamodbav:"userId,omitempty"`
}

// discardable holds the statement-level operators: a call worth deleting, a log line that is not,
// a deferred call worth running early, and an error worth swallowing.
func discardable(ids []string) error {
	defer log.Println("done")

	sort.Strings(ids)
	log.Println("sorted", len(ids))

	err := errors.New("nope")
	if err != nil {
		return err
	}

	return nil
}

func fraction(total float64) float64 {
	return -total * share
}

// slot is a site for the struct operators: two fields of the same shape to exchange, and fields to
// leave out one at a time.
type slot struct {
	PartitionKey string
	SortKey      string
	Reserve      int
}

// store is a site for the call operators — two like arguments to transpose, a context to detach and
// an append whose result is the point of it.
func store(ctx context.Context, partition, sort string, reserve int, into []slot) []slot {
	log.Println(ctx, partition)

	built := slot{
		PartitionKey: partition,
		SortKey:      sort,
		Reserve:      reserve,
	}

	return append(into, built)
}

func forward(ctx context.Context, partition, sort string) []slot {
	return store(ctx, partition, sort, 1, nil)
}

// pump is a site for the channel operators: an unbuffered channel to give room to, a goroutine to
// run inline, and a select with a default to take the default away from.
func pump(values []int) int {
	results := make(chan int)

	go func() {
		for _, value := range values {
			results <- value
		}
		close(results)
	}()

	total := 0
	for {
		select {
		case value, open := <-results:
			if !open {
				return total
			}
			total += value
		default:
			return total
		}
	}
}

func mutable(count, limit int, mask uint, ok bool) int {
	total := count + limit - 1
	total *= limit
	total /= 2
	total += 3
	total -= 4
	total = total*limit/2 - total%7

	mask &= 0xF0
	mask |= 1
	mask ^= 2
	mask &^= 4
	mask <<= 1
	mask >>= 1
	masked := mask&1 | mask&^2 ^ mask<<3>>3

	if count < limit && limit <= 10 || count > 0 && count >= 1 {
		total++
	}
	if count != limit || count == 0 {
		total--
	}
	if !ok {
		return 0
	}

	for range limit {
		if count == 0 {
			continue
		}
		switch prefix + "x" {
		case "slot_x":
			break
		}
		break
	}

	on, off := true, false
	if on && !off {
		return total + 42 + int(masked)
	}

	return total
}
