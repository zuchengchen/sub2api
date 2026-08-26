<template>
  <span
    :class="sizeClasses[size]"
    class="vip-badge inline-flex select-none items-center align-middle"
    :title="t('common.vipBadgeTitle')"
    data-testid="vip-badge"
  >
    <svg
      viewBox="0 0 24 24"
      fill="currentColor"
      aria-hidden="true"
      class="shrink-0"
      :class="iconClasses[size]"
    >
      <path
        d="M12 3.1c.32 0 .62.17.78.45l2.53 4.4 3.97-2.83a.72.72 0 0 1 1.13.8l-2.47 7.94a1.05 1.05 0 0 1-1 .74H7.06a1.05 1.05 0 0 1-1-.74L3.59 5.92a.72.72 0 0 1 1.13-.8l3.97 2.82 2.53-4.39c.16-.28.46-.45.78-.45Z"
      />
      <path
        d="M5.9 16.6h12.2c.36 0 .65.29.65.65v1.15c0 .55-.44 1-1 1H6.25a1 1 0 0 1-1-1v-1.15c0-.36.29-.65.65-.65Z"
      />
    </svg>
    <span class="vip-badge-label">{{ displayLabel }}</span>
    <span class="vip-badge-shine" aria-hidden="true"></span>
  </span>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

const props = withDefaults(
  defineProps<{
    size?: 'xs' | 'sm' | 'md'
    label?: string
  }>(),
  {
    size: 'sm',
    label: ''
  }
)

const { t } = useI18n()

const displayLabel = computed(() => props.label || t('common.vipBadge'))

const sizeClasses = {
  xs: 'gap-1 rounded-md px-2 py-[3px] text-[10px]',
  sm: 'gap-1 rounded-lg px-3 py-1 text-[11px]',
  md: 'gap-1.5 rounded-xl px-4 py-1.5 text-xs'
}

const iconClasses = {
  xs: 'h-2.5 w-2.5',
  sm: 'h-3 w-3',
  md: 'h-3.5 w-3.5'
}
</script>

<style scoped>
.vip-badge {
  position: relative;
  overflow: hidden;
  isolation: isolate;
  border-radius: inherit;
  font-weight: 800;
  letter-spacing: 0.09em;
  color: #451a03;
  background:
    linear-gradient(135deg, #fde68a 0%, #fbbf24 38%, #f59e0b 68%, #d97706 100%);
  box-shadow:
    inset 0 1px 0 rgba(255, 255, 255, 0.65),
    0 1px 2px rgba(180, 83, 9, 0.35),
    0 0 6px rgba(245, 158, 11, 0.35);
}

.dark .vip-badge {
  color: #fffbeb;
  background:
    linear-gradient(135deg, #fcd34d 0%, #f59e0b 42%, #ea8a0a 70%, #b45309 100%);
  box-shadow:
    inset 0 1px 0 rgba(255, 255, 255, 0.5),
    0 1px 3px rgba(0, 0, 0, 0.45),
    0 0 8px rgba(245, 158, 11, 0.45);
}

.vip-badge-label {
  line-height: 1.25;
  padding-right: 0.09em;
}

.vip-badge-shine {
  position: absolute;
  inset: 0;
  overflow: hidden;
  border-radius: inherit;
  pointer-events: none;
}

.vip-badge-shine::before {
  content: '';
  position: absolute;
  top: -60%;
  bottom: -60%;
  left: 0;
  width: 34%;
  background: linear-gradient(
    105deg,
    transparent 0%,
    rgba(255, 255, 255, 0.15) 35%,
    rgba(255, 255, 255, 0.75) 50%,
    rgba(255, 255, 255, 0.15) 65%,
    transparent 100%
  );
  transform: translateX(-260%) skewX(-18deg);
  animation: vip-shine-sweep 3.2s cubic-bezier(0.4, 0, 0.2, 1) infinite;
}

@keyframes vip-shine-sweep {
  0% {
    transform: translateX(-260%) skewX(-18deg);
  }
  45% {
    transform: translateX(420%) skewX(-18deg);
  }
  100% {
    transform: translateX(420%) skewX(-18deg);
  }
}

@media (prefers-reduced-motion: reduce) {
  .vip-badge-shine::before {
    animation: none;
  }
}
</style>
