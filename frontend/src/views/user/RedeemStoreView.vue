<template>
  <AppLayout>
    <div class="redeem-store-layout">
      <section class="redeem-store-shell" :aria-label="t('nav.buyRedeemCode')">
        <div class="redeem-store-toolbar">
          <span class="min-w-0 truncate text-sm font-medium text-gray-700 dark:text-dark-200">
            {{ t('nav.buyRedeemCode') }}
          </span>
          <a
            :href="redeemStoreUrl"
            target="_blank"
            rel="noopener noreferrer"
            class="btn btn-secondary btn-sm flex-shrink-0"
          >
            <Icon name="externalLink" size="sm" class="mr-1.5" :stroke-width="2" />
            {{ t('customPage.openInNewTab') }}
          </a>
        </div>

        <iframe
          :src="redeemStoreUrl"
          :title="t('nav.buyRedeemCode')"
          class="redeem-store-frame"
          allow="clipboard-write; payment"
          referrerpolicy="strict-origin-when-cross-origin"
        ></iframe>
      </section>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'

const redeemStoreUrl = 'https://catfk.com/shop/IGUZN6L4'
const { t } = useI18n()
</script>

<style scoped>
.redeem-store-layout {
  height: calc(100vh - 6rem);
  height: calc(100dvh - 6rem);
  min-height: 24rem;
}

.redeem-store-shell {
  @apply flex h-full min-h-0 flex-col overflow-hidden border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-900;
  border-radius: 0.5rem;
}

.redeem-store-toolbar {
  @apply flex h-12 flex-shrink-0 items-center justify-between gap-3 border-b border-gray-200 px-3 dark:border-dark-700;
}

.redeem-store-frame {
  @apply min-h-0 w-full flex-1 bg-white;
  border: 0;
}

@media (min-width: 768px) {
  .redeem-store-layout {
    height: calc(100vh - 7rem);
    height: calc(100dvh - 7rem);
  }
}

@media (min-width: 1024px) {
  .redeem-store-layout {
    height: calc(100vh - 8rem);
    height: calc(100dvh - 8rem);
  }
}
</style>
