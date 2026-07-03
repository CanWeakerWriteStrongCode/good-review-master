<template>
  <view class="page">
    <view class="card">
      <text class="title">不是好评大师</text>
      <text class="subtitle">监控面板</text>

      <view class="form" v-if="needPassword">
        <input
          class="input"
          v-model="username"
          placeholder="账号"
          @confirm="doLogin"
        />
        <input
          class="input"
          type="password"
          v-model="password"
          placeholder="密码"
          @confirm="doLogin"
        />
        <view class="btn" @click="doLogin">
          <text class="btn-text">{{ loading ? '登录中...' : '登录' }}</text>
        </view>
        <text class="error" v-if="errMsg">{{ errMsg }}</text>
      </view>

      <view class="form" v-else>
        <view class="btn" @click="enterApp">
          <text class="btn-text">进入面板</text>
        </view>
      </view>
    </view>
  </view>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { login } from '@/api'
import { useAuthStore } from '@/stores/auth'

const store = useAuthStore()
const username = ref('')
const password = ref('')
const loading = ref(false)
const errMsg = ref('')
const needPassword = ref(true)

// 先尝试无密码访问
async function checkNeedPassword() {
  try {
    const res = await login('', '')
    if (res.need_password === false) {
      needPassword.value = false
      store.setNeedPassword(false)
    }
  } catch (_) {
    needPassword.value = true
  }
}

async function doLogin() {
  if (!username.value) {
    errMsg.value = '请输入账号'
    return
  }
  if (!password.value) {
    errMsg.value = '请输入密码'
    return
  }
  loading.value = true
  errMsg.value = ''
  try {
    const res = await login(username.value, password.value)
    if (res.token) {
      store.setToken(res.token)
      store.setNeedPassword(true)
      uni.reLaunch({ url: '/pages/groups/index' })
    }
  } catch (e: any) {
    errMsg.value = e.message || '登录失败'
  } finally {
    loading.value = false
  }
}

function enterApp() {
  uni.reLaunch({ url: '/pages/groups/index' })
}

checkNeedPassword()
</script>

<style scoped>
.page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background: linear-gradient(135deg, #1a1a2e, #16213e);
}

.card {
  width: 320px;
  padding: 40px 30px;
  background: #fff;
  border-radius: 12px;
  box-shadow: 0 8px 30px rgba(0, 0, 0, 0.3);
}

.title {
  display: block;
  text-align: center;
  font-size: 22px;
  font-weight: 700;
  color: #1a1a2e;
}

.subtitle {
  display: block;
  text-align: center;
  font-size: 13px;
  color: #999;
  margin-top: 4px;
  margin-bottom: 30px;
}

.form {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.input {
  height: 44px;
  padding: 0 14px;
  border: 1px solid #ddd;
  border-radius: 8px;
  font-size: 15px;
  color: #333;
  background: #f8f9fb;
}

.input:focus {
  border-color: #1a1a2e;
}

.btn {
  height: 44px;
  display: flex;
  align-items: center;
  justify-content: center;
  background: linear-gradient(135deg, #1a1a2e, #16213e);
  border-radius: 8px;
}

.btn:active {
  opacity: 0.85;
}

.btn-text {
  color: #fff;
  font-size: 16px;
  font-weight: 600;
}

.error {
  text-align: center;
  color: #e74c3c;
  font-size: 13px;
}
</style>
