// @license
// Copyright (C) 2022  Dinko Korunic
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package db

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/dkorunic/e-dnevnik-bot/logger"
)

const (
	DefaultDBPath       = ".e-dnevnik.db"  // default BadgerDB folder
	DefaultTTL          = time.Hour * 9000 // a bit more than 1 year TTL
	DefaultDiscardRatio = 0.5              // recommended discard ratio from Badger docs
)

// Edb holds e-dnevnik structure including Bardger struct.
type Edb struct {
	db         *badger.DB
	isExisting bool // already created/initialized db
}

// New opens a new database, flagging if the database already preexisting.
func New(filePath string) (*Edb, error) {
	if filePath == "" {
		filePath = DefaultDBPath
	}

	isExisting := dbExists(filePath)

	logger.Debug().Msgf("Opening database: %v", filePath)
	opts := badger.DefaultOptions(filePath)

	// adapt for low memory environment
	//nolint:mnd
	if strconv.IntSize == 32 {
		logger.Info().Msg("Detected 32-bit environment, tuning DB for lower memory usage")

		opts.ValueLogFileSize = 1 << 20 //nolint:mnd
	}

	db, err := badger.Open(opts.WithLogger(nil)) // disable Badger verbose logging
	if err != nil {
		if isExisting {
			return nil, fmt.Errorf("could not open database: %w", err)
		}

		return nil, fmt.Errorf("could not create database: %w", err)
	}

	edb := &Edb{db: db, isExisting: isExisting}

	return edb, nil
}

// Close closes database, optionally running GC (removing state data from value log file).
func (db *Edb) Close() error {
	logger.Debug().Msg("Running database GC")
again:
	err := db.db.RunValueLogGC(DefaultDiscardRatio)

	if err == nil {
		goto again
	}

	logger.Debug().Msg("Closing database")

	return db.db.Close()
}

// CheckAndFlag checks if a key already exists in the database and marks it with a flag
// if it doesn't exist. The flag is set with a TTL of 1+ year.
//
// The key is created by hashing a concatenation of the bucket, subBucket and target
// strings using SHA-256.
//
// If the key already exists, the function returns (true, nil). If the key doesn't
// exist, the function marks the key and returns (false, nil) on success or
// (false, error) on error.
func (db *Edb) CheckAndFlag(bucket, subBucket string, target []string) (bool, error) {
	// SHA256 hash of (bucket, subBucket, []target)
	key := hashContent(bucket, subBucket, target)

	var found bool

	// check if key exists
	err := db.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(key)

		switch {
		// key not found (found=false)
		case errors.Is(err, badger.ErrKeyNotFound):
			return nil
		// key found (found=true)
		case err == nil:
			found = true

			return nil
		}

		// all other errors (found=false)
		return err
	})

	if err != nil {
		// return quickly: (fatal) error + found=false
		return false, err
	} else if found {
		// return quickly: no error + found=true
		return true, nil
	}

	// key hasn't been found yet, so mark the key and set 1+year TTL
	err = db.db.Update(func(txn *badger.Txn) error {
		e := badger.NewEntry(key, []byte("")).WithTTL(DefaultTTL)

		return txn.SetEntry(e)
	})

	// found=false
	return false, err
}

// Existing returns if the database was freshly initialized.
func (db *Edb) Existing() bool {
	return db.isExisting
}

// FetchAndDelete fetches a value by key, deletes the key and returns the value.
//
// It does the following steps:
//
// 1. Finds the key in the database.
// 2. Copies the associated value.
// 3. Deletes the key.
//
// If any of the steps fail, it will return an error and the value will not be copied.
//
// The returned value is valid until the next database GC is run.
func (db *Edb) FetchAndDelete(key []byte) ([]byte, error) {
	var val []byte

	err := db.db.Update(func(txn *badger.Txn) error {
		// find key -- returns error if key doesn't exist
		item, err := txn.Get(key)
		if err != nil {
			return err
		}

		// store value
		val, err = item.ValueCopy(val)
		if err != nil {
			return err
		}

		// delete key
		err = txn.Delete(key)

		return err
	})

	return val, err
}

// FetchAndStore fetches a value by key, applies a given function to the value
// and stores the result.
//
// It does the following steps:
//
// 1. Finds the key in the database.
// 2. Copies the associated value.
// 3. Calls the given function with the copied value as argument and stores the result.
// 4. Stores the result in the database with the same key and a TTL of 1+ year.
//
// If any of the steps fail, it will return an error.
func (db *Edb) FetchAndStore(key []byte, f func(old []byte) ([]byte, error)) error {
	var val, newVal []byte

	err := db.db.Update(func(txn *badger.Txn) error {
		// find key -- doesn't return error if key doesn't exist
		item, err := txn.Get(key)
		if err != nil && !errors.Is(err, badger.ErrKeyNotFound) {
			return err
		}

		// copy value (if it exists)
		if item != nil {
			val, err = item.ValueCopy(val)
			if err != nil {
				return err
			}
		}

		// call conversion function
		newVal, err = f(val)
		if err != nil {
			return err
		}

		e := badger.NewEntry(key, newVal).WithTTL(DefaultTTL)

		return txn.SetEntry(e)
	})

	return err
}
