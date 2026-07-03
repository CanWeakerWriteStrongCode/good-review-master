package server

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// TokenStore 内存 token 仓库，用于管理面板的登录会话
type TokenStore struct {
	tokens sync.Map // token → struct{}
}

// NewTokenStore 创建新的 token 仓库
func NewTokenStore() *TokenStore {
	return &TokenStore{}
}

// Generate 生成随机 token 并存入仓库
func (ts *TokenStore) Generate() string {
	b := make([]byte, 16)
	rand.Read(b)
	token := hex.EncodeToString(b)
	ts.tokens.Store(token, struct{}{})
	return token
}

// Exists 检查 token 是否有效
func (ts *TokenStore) Exists(token string) bool {
	_, ok := ts.tokens.Load(token)
	return ok
}

// Remove 删除 token（退出登录）
func (ts *TokenStore) Remove(token string) {
	ts.tokens.Delete(token)
}
