package kv

import (
	"context"
	"testing"
	"time"
)

func TestRistretto读写与批量读(t *testing.T) {
	cache, err := NewRistretto(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx := context.Background()

	if err := cache.Set(ctx, "a", []byte("1"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := cache.Set(ctx, "b", []byte("2"), time.Minute); err != nil {
		t.Fatal(err)
	}
	cache.Wait()

	if value, ok, err := cache.Get(ctx, "a"); err != nil || !ok || string(value) != "1" {
		t.Fatalf("Get(a) = %q, %v, %v", value, ok, err)
	}
	all, err := cache.GetMany(ctx, []string{"a", "b", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if string(all["a"]) != "1" || string(all["b"]) != "2" {
		t.Fatalf("GetMany = %#v", all)
	}
	if _, ok := all["missing"]; ok {
		t.Fatal("未命中的键不应出现在结果里")
	}

	if err := cache.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	cache.Wait()
	if _, ok, _ := cache.Get(ctx, "a"); ok {
		t.Fatal("删除后不应命中")
	}
}

func TestRistrettoTTL过期(t *testing.T) {
	cache, err := NewRistretto(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx := context.Background()

	if err := cache.Set(ctx, "short", []byte("x"), 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	cache.Wait()
	time.Sleep(60 * time.Millisecond)
	if _, ok, _ := cache.Get(ctx, "short"); ok {
		t.Fatal("TTL 到期后不应命中")
	}
}

func TestNoop永远未命中(t *testing.T) {
	var cache KV = Noop{}
	if err := cache.Set(context.Background(), "a", []byte("1"), 0); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.Get(context.Background(), "a"); err != nil || ok {
		t.Fatalf("Noop Get = %v, %v", ok, err)
	}
}
