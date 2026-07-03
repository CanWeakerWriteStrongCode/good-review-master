import { defineStore } from 'pinia'
import { ref } from 'vue'

const TOKEN_KEY = 'good_review_token'

export const useAuthStore = defineStore('auth', () => {
  const token = ref<string>(localStorage.getItem(TOKEN_KEY) || '')
  const needPassword = ref<boolean>(true)

  function setToken(val: string) {
    token.value = val
    localStorage.setItem(TOKEN_KEY, val)
  }

  function setNeedPassword(val: boolean) {
    needPassword.value = val
  }

  function isAuthenticated() {
    return !needPassword.value || !!token.value
  }

  function logout() {
    token.value = ''
    localStorage.removeItem(TOKEN_KEY)
  }

  return { token, needPassword, setToken, setNeedPassword, isAuthenticated, logout }
})
