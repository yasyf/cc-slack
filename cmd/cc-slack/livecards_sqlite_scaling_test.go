package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveCardSQLiteMutationWriteAmplification(t *testing.T) {
	for _, count := range []int{1, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			now := time.Now()
			cards := &liveCards{
				path: filepath.Join(t.TempDir(), "live-cards.db"),
				now:  func() time.Time { return now },
				log:  log.New(io.Discard, "", 0),
			}
			if err := cards.load(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cards.close(); err != nil {
					t.Error(err)
				}
			})
			keys := make([]string, 0, count)
			for i := range count {
				thread := fmt.Sprint(i)
				key := threadKey("D1", thread)
				cards.threads[key] = &liveThread{
					Channel: "D1", Thread: thread, Updated: now,
					Root:    &liveMessage{Card: statusCard{Title: strings.Repeat("x", 2048)}},
					Answers: map[string]askAnswer{"ask": {}},
				}
				keys = append(keys, key)
			}
			cards.save(keys, nil)
			if len(cards.dirtyThreads) != 0 {
				t.Fatal("fixture persistence failed")
			}
			if _, err := cards.st.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				t.Fatal(err)
			}
			var before, after int
			if err := cards.st.DB().QueryRow("SELECT total_changes()").Scan(&before); err != nil {
				t.Fatal(err)
			}
			cards.answerDrawn("D1", "0", "ask")
			if err := cards.st.DB().QueryRow("SELECT total_changes()").Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after-before != 1 {
				t.Fatalf("one answer changed %d rows", after-before)
			}
			wal, err := os.Stat(cards.path + "-wal")
			if err != nil {
				t.Fatal(err)
			}
			if wal.Size() == 0 || wal.Size() > 64<<10 {
				t.Fatalf("one answer wrote %d WAL bytes with %d retained threads", wal.Size(), count)
			}
			t.Logf("threads=%d rows_changed=%d wal_bytes=%d", count, after-before, wal.Size())
		})
	}
}
