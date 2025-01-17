// bagdb: Simple datastorage
// Copyright 2021 billy authors
// SPDX-License-Identifier: BSD-3-Clause

package billy

import (
	"fmt"
	"io"
	"sort"
)

type Database interface {
	io.Closer

	// Put stores the data to the underlying database, and returns the key needed
	// for later accessing the data.
	// The data is copied by the database, and is safe to modify after the method returns
	Put(data []byte) (uint64, error)

	// Get retrieves the data stored at the given key.
	Get(key uint64) ([]byte, error)

	// Delete marks the data for deletion, which means it will (eventually) be
	// overwritten by other data. After calling Delete with a given key, the results
	// from doing Get(key) is undefined -- it may return the same data, or some other
	// data, or fail with an error.
	Delete(key uint64) error

	// Limits returns the smallest and largest slot size.
	Limits() (uint32, uint32)
}

// SlotSizeFn is a method that acts as a "generator": a closure which, at each
// invocation, should spit out the next slot-size. In order to create a database with three
// shelves invocation of the method should return e.g.
//
//	10, false
//	20, false
//	30, true
//
// OBS! The slot size must take item header size (4 bytes) into account. So if you
// plan to store 120 bytes, then the slot needs to be at least 124 bytes large.
type SlotSizeFn func() (size uint32, done bool)

// SlotSizePowerOfTwo is a SlotSizeFn which arranges the slots in shelves which
// double in size for each level.
func SlotSizePowerOfTwo(min, max uint32) SlotSizeFn {
	if min >= max { // programming error
		panic(fmt.Sprintf("Bad options, min (%d) >= max (%d)", min, max))
	}
	v := min
	return func() (uint32, bool) {
		ret := v
		v += v
		return ret, ret >= max
	}
}

// SlotSizeLinear is a SlotSizeFn which arranges the slots in shelves which
// increase linearly.
func SlotSizeLinear(size, count int) SlotSizeFn {
	i := 1
	return func() (uint32, bool) {
		ret := size * i
		i++
		return uint32(ret), i >= count
	}
}

type database struct {
	shelves []*shelf
}

type Options struct {
	Path     string
	Readonly bool
	Snappy   bool // unused for now
}

// OpenCustom opens a (new or eixsting) database, with configurable limits. The
// given slotSizeFn will be used to determine both the shelf sizes and the number
// of shelves.
// The function must yield values in increasing order.
// If shelf already exists, they are opened and read, in order to populate the
// internal gap-list.
// While doing so, it's a good opportunity for the caller to read the data out,
// (which is probably desirable), which can be done using the optional onData callback.
func Open(opts Options, slotSizeFn SlotSizeFn, onData OnDataFn) (Database, error) {
	var (
		db           = &database{}
		prevSlotSize uint32
		prevId       int
		slotSize     uint32
		done         bool
	)
	for !done {
		slotSize, done = slotSizeFn()
		if slotSize <= prevSlotSize {
			return nil, fmt.Errorf("slot sizes must be in increasing order")
		}
		prevSlotSize = slotSize
		shelfet, err := openShelf(opts.Path, slotSize, wrapShelfDataFn(len(db.shelves), onData), opts.Readonly)
		if err != nil {
			db.Close() // Close shelves
			return nil, err
		}
		db.shelves = append(db.shelves, shelfet)

		if id := len(db.shelves) & 0xfff; id < prevId {
			return nil, fmt.Errorf("too many shelves (%d)", len(db.shelves))
		} else {
			prevId = id
		}
	}
	return db, nil
}

// Put stores the data to the underlying database, and returns the key needed
// for later accessing the data.
// The data is copied by the database, and is safe to modify after the method returns
func (db *database) Put(data []byte) (uint64, error) {
	// Search uses binary search to find and return the smallest index i
	// in [0, n) at which f(i) is true,
	index := sort.Search(len(db.shelves), func(i int) bool {
		return len(data)+itemHeaderSize <= int(db.shelves[i].slotSize)
	})
	if index == len(db.shelves) {
		return 0, fmt.Errorf("no shelf found for size %d", len(data))
	}
	if slot, err := db.shelves[index].Put(data); err != nil {
		return 0, err
	} else {
		return slot | uint64(index)<<28, nil
	}
}

// Get retrieves the data stored at the given key.
func (db *database) Get(key uint64) ([]byte, error) {
	id := int(key>>28) & 0xfff
	return db.shelves[id].Get(key & 0x0FFFFFFF)
}

// Delete marks the data for deletion, which means it will (eventually) be
// overwritten by other data. After calling Delete with a given key, the results
// from doing Get(key) is undefined -- it may return the same data, or some other
// data, or fail with an error.
func (db *database) Delete(key uint64) error {
	id := int(key>>28) & 0xfff
	return db.shelves[id].Delete(key & 0x00FFFFFF)
}

// OnDataFn is used to iterate the entire dataset in the database.
// After the method returns, the content of 'data' will be modified by
// the iterator, so it needs to be copied if it is to be used later.
type OnDataFn func(key uint64, data []byte)

func wrapShelfDataFn(shelfId int, onData OnDataFn) onShelfDataFn {
	if onData == nil {
		return nil
	}
	return func(slot uint64, data []byte) {
		key := slot | uint64(shelfId)<<28
		onData(key, data)
	}
}

// Iterate iterates through all the data in the database, and invokes the
// given onData method for every element
func (db *database) Iterate(onData OnDataFn) {
	for i, b := range db.shelves {
		b.Iterate(wrapShelfDataFn(i, onData))
	}
}

func (db *database) Limits() (uint32, uint32) {
	smallest := db.shelves[0].slotSize
	largest := db.shelves[len(db.shelves)-1].slotSize
	return smallest, largest
}

// Close implements io.Closer
func (db *database) Close() error {
	var err error
	for _, shelf := range db.shelves {
		if e := shelf.Close(); e != nil {
			err = e
		}
	}
	return err
}
