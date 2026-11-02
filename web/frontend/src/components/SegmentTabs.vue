<template>
  <view class="segment">
    <view
      v-for="opt in options"
      :key="opt.key"
      class="segment-item"
      :class="{ active: opt.key === modelValue }"
      @click="pick(opt.key)"
    >
      <text class="segment-label">{{ opt.label }}</text>
      <text class="segment-badge" v-if="opt.badge">{{ opt.badge }}</text>
    </view>
  </view>
</template>

<script lang="ts">
/** 顶部选项条的一项：key 给业务判断，label 显示，badge 是右侧的小数字（可选） */
export interface SegmentOption {
  key: string
  label: string
  badge?: string | number
}
</script>

<script setup lang="ts">
defineProps<{ modelValue: string; options: SegmentOption[] }>()

const emit = defineEmits<{ (e: 'update:modelValue', value: string): void }>()

function pick(key: string) {
  emit('update:modelValue', key)
}
</script>

<style scoped>
.segment {
  display: flex;
  gap: 6px;
  background: #e4e7ee;
  border-radius: 12px;
  padding: 5px;
  margin-bottom: 14px;
}
.segment-item {
  flex: 1;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 5px;
  padding: 9px 0;
  border-radius: 9px;
  border: 1px solid transparent;
  transition: background 0.15s, color 0.15s;
}
/* 选中态实心蓝底白字，跟未选中的灰字彻底分开 */
.segment-item.active {
  background: #4a9fd4;
  border-color: #3d8cbd;
  box-shadow: 0 2px 6px rgba(74, 159, 212, 0.35);
}
.segment-label {
  font-size: 13px;
  color: #6b7280;
}
.segment-item.active .segment-label {
  color: #fff;
  font-weight: 600;
}
.segment-badge {
  font-size: 11px;
  color: #6b7280;
  background: #f4f6f9;
  border-radius: 9px;
  padding: 0 6px;
  min-width: 18px;
  text-align: center;
}
.segment-item.active .segment-badge {
  color: #fff;
  background: rgba(255, 255, 255, 0.25);
}
</style>
