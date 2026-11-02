<template>
  <view class="page">
    <view class="card">
      <text class="title">{{ t('login.title') }}</text>
      <text class="subtitle">{{ t('login.subtitle') }}</text>

      <view class="form">
        <input
          class="input"
          v-model="username"
          :placeholder="t('login.username')"
          @confirm="doLogin"
        />
        <input
          class="input"
          type="password"
          v-model="password"
          :placeholder="t('login.password')"
          @confirm="doLogin"
        />
        <view class="btn" @click="doLogin">
          <text class="btn-text">{{ loading ? t('login.submitting') : t('login.submit') }}</text>
        </view>
        <text class="error" v-if="errMsg">{{ errMsg }}</text>
      </view>

      <!-- 登录页没有顶栏，语言切换放这里：否则一进错语言就没处换 -->
      <view class="locale-row">
        <text class="locale-switch" @click="toggle">{{ isZh ? 'EN' : '中文' }}</text>
      </view>
    </view>
  </view>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { login } from '@/api'
import { useAuthStore } from '@/stores/auth'
import { useLocale, usePageTitle } from '@/composables/useLocale'

const { t, isZh, toggle } = useLocale()
// 登录页没有顶栏，标签页标题得自己同步
usePageTitle(() => t('login.title'))
const store = useAuthStore()
const username = ref('')
const password = ref('')
const loading = ref(false)
const errMsg = ref('')

async function doLogin() {
  if (!username.value) {
    errMsg.value = t('login.usernameRequired')
    return
  }
  if (!password.value) {
    errMsg.value = t('login.passwordRequired')
    return
  }
  loading.value = true
  errMsg.value = ''
  try {
    const res = await login(username.value, password.value)
    if (res.token) {
      store.setToken(res.token)
      uni.reLaunch({ url: '/pages/monitor/index' })
    }
  } catch (e: any) {
    errMsg.value = e.message || t('login.failed')
  } finally {
    loading.value = false
  }
}
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

.locale-row {
  display: flex;
  justify-content: center;
  margin-top: 18px;
}
.locale-switch {
  font-size: 12px;
  color: #8a94a6;
  padding: 4px 12px;
  border: 1px solid #e2e6ee;
  border-radius: 11px;
}
.locale-switch:active {
  background: #f2f4f8;
}
</style>
