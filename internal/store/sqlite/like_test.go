package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ignvex/HomehubExtension/internal/store"
)

// 搜索 "50%" 应该找到标题里真的有 "50%" 的条目。
// escapeLike 把 % 转成 \%，但 SQL 没有 ESCAPE 子句时反斜杠只是普通字符，
// 于是模式变成「含反斜杠、再跟两个任意字符」，一条也匹配不到。
func TestSearchWithLikeMetacharacters(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "e.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	now := time.Now().Unix()
	feed, _, err := db.Feed().Upsert(ctx, &store.Feed{
		URL: "https://example.com/f.xml", Source: "hub", Enabled: true,
		FetchInterval: 1800, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	titles := []string{
		"打折 50% 起",
		"snake_case 命名",
		"普通标题",
	}
	for i, title := range titles {
		_, err := db.Entry().InsertIfNew(ctx, &store.Entry{
			FeedID: feed.ID, GUID: "g" + title + string(rune('a'+i)),
			Title: title, FetchedAt: now, ContentText: title,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		q    string
		want int
	}{
		{"50%", 1},
		{"snake_case", 1},
		{"50", 1},
		{"普通", 1},
		{"不存在", 0},
	}

	for _, tc := range cases {
		got, _, err := db.Entry().List(ctx, store.EntryQuery{Search: tc.q, Limit: 50})
		if err != nil {
			t.Fatalf("q=%q: %v", tc.q, err)
		}
		if len(got) != tc.want {
			t.Errorf("q=%q 命中 %d 条, want %d", tc.q, len(got), tc.want)
		}
	}
}
