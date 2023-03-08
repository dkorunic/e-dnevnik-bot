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

package main

import (
	"fmt"
	"log"
	"math/rand"

	"filippo.io/mostly-harmless/cryptosource"
	"github.com/dgraph-io/badger/v4"
)

const (
	DefaultDBName = ".e-dnevnik.db"
)

func main() {
	db, err := badger.Open(badger.DefaultOptions(DefaultDBName).WithLogger(nil))
	if err != nil {
		log.Fatalf("Could not open database: %v\n", err)
	}
	defer db.Close()

	// fetch all keys
	var keys [][]byte
	err = db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			k := item.Key()
			keys = append(keys, k)
		}

		return nil
	})
	if err != nil {
		log.Fatalf("Could not list keys: %v\n", err)
	}

	totalKeys := len(keys)

	fmt.Printf("Found total of %v keys.\n", totalKeys)

	r := rand.New(cryptosource.New())

	// delete two random keys
	for i := 0; i < 2; i++ {
		randIdx := r.Intn(totalKeys)
		randKey := keys[randIdx]
		fmt.Printf("Deleting key number %v: %v\n", randIdx, randKey)

		// delete key from keys slice
		keys = append(keys[:randIdx], keys[randIdx+1:]...)

		// delete k/v from DB
		err = db.Update(func(txn *badger.Txn) error {
			return txn.Delete(randKey)
		})

		if err != nil {
			log.Fatalf("Unable to delete key: %v", err)
		}

	}
}
