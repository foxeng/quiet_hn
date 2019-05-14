package main

import (
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/foxeng/quiet_hn/hn"
)

const timeout = 15 * time.Minute

var memo cache

type item struct {
	hn.Item
	Host string
	rank int
}

type templateData struct {
	Stories []item
	Time    time.Duration
}

type cacheItem struct {
	since time.Time
	it *item
}

type cache struct {
	lock sync.RWMutex
	store map[int]cacheItem
}

func main() {
	// parse flags
	var port, numStories int
	flag.IntVar(&port, "port", 3000, "the port to start the web server on")
	flag.IntVar(&numStories, "num_stories", 30, "the number of top stories to display")
	flag.Parse()

	tpl := template.Must(template.ParseFiles("./index.gohtml"))

	http.HandleFunc("/", handler(numStories, tpl))

	// Start the server
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), nil))
}

func handler(numStories int, tpl *template.Template) http.HandlerFunc {
	// Initialize the cache
	memo.store = make(map[int]cacheItem)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var client hn.Client
		ids, err := client.TopItems()
		if err != nil {
			http.Error(w, "Failed to load top stories", http.StatusInternalServerError)
			return
		}

		// An approach with zero extra work (no prefetching):
		// Spawn numStories concurrent fetches
		// For each one which returns not-a-story, spawn another fetch
		// To order the items (since we cannot rely on the presence of item.Score), we
		// channel the index (in ids) of each item back and forth in the goroutines
		storiesm := make(map[int]*item)
		var rank int
		c := make(chan *item)
		for rank = 0; rank < numStories; rank++ {
			go fetchItem(&client, rank, ids[rank], c)
		}

		for len(storiesm) < numStories {
			it := <-c
			if it == nil {
				go fetchItem(&client, rank, ids[rank], c)
				rank++
			} else {
				storiesm[it.rank] = it
			}
		}

		stories := mapToSlice(storiesm)
		data := templateData{
			Stories: stories,
			Time:    time.Now().Sub(start),
		}
		err = tpl.Execute(w, data)
		if err != nil {
			http.Error(w, "Failed to process the template", http.StatusInternalServerError)
			return
		}
	})
}

func mapToSlice(m map[int]*item) []item {
	var keys []int
	for k, _ := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	items := make([]item, len(m))
	for i, k := range keys {
		items[i] = *m[k]
	}
	return items
}

func fetchItem(client *hn.Client, rank int, id int, out chan<- *item) {
	// Check the cache
	memo.lock.RLock()
	res, ok := memo.store[id]
	memo.lock.RUnlock()
	if ok && time.Since(res.since) < timeout {
		out <- res.it
		return
	}

	hnItem, err := client.GetItem(id)
	if err != nil {
		out <- nil
		return
	}
	it := parseHNItem(hnItem)
	if !isStoryLink(it) {
		out <- nil
		return
	}
	it.rank = rank

	// Update the cache
	// TODO OPT: Duplicate suppression (see 'The Go Programming Language' p. 276)
	memo.lock.Lock()
	memo.store[id] = cacheItem{time.Now(), &it}
	memo.lock.Unlock()

	out <- &it
}

func isStoryLink(item item) bool {
	return item.Type == "story" && item.URL != ""
}

func parseHNItem(hnItem hn.Item) item {
	ret := item{Item: hnItem}
	url, err := url.Parse(ret.URL)
	if err == nil {
		ret.Host = strings.TrimPrefix(url.Hostname(), "www.")
	}
	return ret
}
