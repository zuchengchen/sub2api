<template>
  <AppLayout>
    <div class="space-y-7" data-test="risk-control-view">
      <div v-if="loading" class="flex min-h-64 items-center justify-center">
        <div class="h-8 w-8 animate-spin rounded-full border-b-2 border-primary-600"></div>
      </div>

      <template v-else>
        <header class="flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
          <div>
            <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">
              {{ t('admin.riskControl.title') }}
            </h1>
            <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
              {{ t('admin.riskControl.description') }}
            </p>
          </div>
          <div class="flex flex-wrap gap-2">
            <button
              type="button"
              class="btn btn-secondary inline-flex items-center gap-2"
              :disabled="refreshing"
              :title="t('admin.riskControl.refresh')"
              @click="refreshAll"
            >
              <Icon name="refresh" size="sm" :class="refreshing ? 'animate-spin' : ''" />
              {{ t('admin.riskControl.refresh') }}
            </button>
            <button
              type="button"
              class="btn btn-primary inline-flex items-center gap-2"
              :disabled="saving"
              data-test="save-risk-control"
              @click="saveConfig"
            >
              <Icon name="check" size="sm" />
              {{ saving ? t('common.saving') : t('admin.riskControl.saveConfig') }}
            </button>
          </div>
        </header>

        <div
          class="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-[repeat(11,minmax(0,1fr))]"
          data-test="risk-overview"
        >
          <div
            v-for="item in overviewItems"
            :key="item.key"
            class="rounded-lg border border-gray-100 bg-white px-4 py-3 shadow-sm dark:border-dark-700 dark:bg-dark-800"
            :class="item.key === 'second-layer-cache' ? 'xl:col-span-3' : 'xl:col-span-2'"
            :data-test="`risk-overview-${item.key}`"
            :data-cache-hits="item.cacheMetrics?.hits"
            :data-cache-misses="item.cacheMetrics?.misses"
            :data-cache-writes="item.cacheMetrics?.writes"
            :data-cache-errors="item.cacheMetrics?.errors"
          >
            <div class="flex items-center gap-3">
              <div class="flex h-9 w-9 flex-shrink-0 items-center justify-center rounded-lg" :class="item.iconClass">
                <Icon :name="item.icon" size="sm" />
              </div>
              <div class="min-w-0">
                <p class="text-xs font-medium text-gray-500 dark:text-gray-400">{{ item.label }}</p>
                <p class="mt-1 truncate text-lg font-semibold text-gray-900 dark:text-white">{{ item.value }}</p>
                <p class="truncate text-xs text-gray-500 dark:text-gray-400" :title="item.meta">{{ item.meta }}</p>
              </div>
            </div>
          </div>
        </div>

        <section class="border-y border-gray-200 py-6 dark:border-dark-700" aria-labelledby="reviewers-heading">
          <div class="mb-4 flex flex-col gap-2 sm:flex-row sm:items-end sm:justify-between">
            <div>
              <h2 id="reviewers-heading" class="text-lg font-semibold text-gray-900 dark:text-white">
                {{ t('admin.riskControl.reviewersTitle') }}
              </h2>
              <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
                {{ t('admin.riskControl.reviewersSummary') }}
              </p>
            </div>
            <div class="flex items-center gap-3">
              <span class="text-sm text-gray-600 dark:text-gray-300">{{ t('admin.riskControl.enabled') }}</span>
              <Toggle v-model="configForm.enabled" data-test="risk-control-enabled" />
            </div>
          </div>

          <div class="grid grid-cols-1 gap-4 lg:grid-cols-2">
            <article class="rounded-lg border border-gray-200 bg-white p-5 dark:border-dark-700 dark:bg-dark-800">
              <div class="flex items-start justify-between gap-4">
                <div class="flex min-w-0 items-start gap-3">
                  <span
                    class="flex h-10 w-10 flex-shrink-0 items-center justify-center rounded-lg bg-emerald-50 text-emerald-700 dark:bg-emerald-900/20 dark:text-emerald-300"
                  >
                    <Icon name="cloud" size="md" />
                  </span>
                  <div class="min-w-0">
                    <h3 class="font-semibold text-gray-900 dark:text-white">DeepSeek V4 Flash</h3>
                    <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
                      {{
                        t('admin.riskControl.deepseekReviewerMeta', {
                          threshold: percent(configForm.deepseek_threshold),
                        })
                      }}
                    </p>
                  </div>
                </div>
                <Toggle v-model="configForm.deepseek_enabled" data-test="deepseek-enabled" />
              </div>
              <div class="mt-4 flex flex-wrap gap-2 text-xs">
                <span class="rounded-md bg-gray-100 px-2 py-1 text-gray-600 dark:bg-dark-700 dark:text-gray-300">
                  {{ t('admin.riskControl.nonThinking') }}
                </span>
                <span class="rounded-md bg-gray-100 px-2 py-1 text-gray-600 dark:bg-dark-700 dark:text-gray-300">
                  {{ t('admin.riskControl.textOnly') }}
                </span>
                <span
                  class="rounded-md px-2 py-1"
                  :class="healthyChannelCount > 0 ? statusClasses.healthy : statusClasses.unknown"
                >
                  {{
                    t('admin.riskControl.channelHealthSummary', {
                      healthy: healthyChannelCount,
                      enabled: enabledChannelCount,
                    })
                  }}
                </span>
              </div>
            </article>

            <article class="rounded-lg border border-gray-200 bg-white p-5 dark:border-dark-700 dark:bg-dark-800">
              <div class="flex items-start justify-between gap-4">
                <div class="flex min-w-0 items-start gap-3">
                  <span
                    class="flex h-10 w-10 flex-shrink-0 items-center justify-center rounded-lg bg-sky-50 text-sky-700 dark:bg-sky-900/20 dark:text-sky-300"
                  >
                    <Icon name="server" size="md" />
                  </span>
                  <div class="min-w-0">
                    <h3 class="font-semibold text-gray-900 dark:text-white">YuFeng XGuard</h3>
                    <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
                      {{ t('admin.riskControl.yufengReviewerMeta') }}
                    </p>
                  </div>
                </div>
                <Toggle v-model="configForm.yufeng_enabled" data-test="yufeng-enabled" />
              </div>
              <div class="mt-4 text-xs text-gray-500 dark:text-gray-400">
                {{
                  configForm.yufeng_enabled
                    ? t('admin.riskControl.reviewerEnabled')
                    : t('admin.riskControl.reviewerDisabled')
                }}
              </div>
            </article>
          </div>
        </section>

        <section aria-labelledby="stages-heading">
          <div class="mb-4">
            <h2 id="stages-heading" class="text-lg font-semibold text-gray-900 dark:text-white">
              {{ t('admin.riskControl.layerStagesTitle') }}
            </h2>
            <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
              {{ t('admin.riskControl.layerStagesSummary') }}
            </p>
          </div>

          <div class="divide-y divide-gray-200 border-y border-gray-200 dark:divide-dark-700 dark:border-dark-700">
            <div
              v-for="layer in layerRows"
              :key="layer.id"
              class="flex flex-col gap-3 py-4 sm:flex-row sm:items-center sm:justify-between"
              :data-test="`${layer.id}-stage-row`"
            >
              <div>
                <p class="font-medium text-gray-900 dark:text-white">{{ layer.label }}</p>
                <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ layer.meta }}</p>
              </div>
              <div
                class="inline-flex w-fit rounded-lg bg-gray-100 p-1 dark:bg-dark-800"
                role="group"
                :aria-label="layer.label"
              >
                <button
                  v-for="stage in stageOptions"
                  :key="stage.value"
                  type="button"
                  class="min-h-9 min-w-24 rounded-md px-3 py-1.5 text-sm font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-45"
                  :class="
                    layer.stage === stage.value
                      ? stage.activeClass
                      : 'text-gray-500 hover:text-gray-900 dark:text-gray-400 dark:hover:text-white'
                  "
                  :disabled="stage.value === 'enforce' && layer.id === 'layer2' && !secondLayerEnforceReady"
                  :data-test="`${layer.id}-stage-${stage.value}`"
                  @click="setLayerStage(layer.id, stage.value)"
                >
                  {{ stage.label }}
                </button>
              </div>
            </div>
          </div>
          <div
            class="mt-3 flex items-start gap-2 rounded-lg px-3 py-2 text-sm"
            :class="secondLayerEnforceReady ? statusClasses.healthy : statusClasses.warning"
            data-test="enforce-health-gate"
          >
            <Icon
              :name="secondLayerEnforceReady ? 'checkCircle' : 'exclamationTriangle'"
              size="sm"
              class="mt-0.5 flex-shrink-0"
            />
            <span>{{ enforceGateText }}</span>
          </div>
        </section>

        <section aria-labelledby="channels-heading" data-test="deepseek-channels">
          <div class="mb-4 flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
            <div>
              <h2 id="channels-heading" class="text-lg font-semibold text-gray-900 dark:text-white">
                {{ t('admin.riskControl.deepseekChannelsTitle') }}
              </h2>
              <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
                {{ t('admin.riskControl.deepseekChannelsSummary') }}
              </p>
            </div>
            <button
              type="button"
              class="btn btn-secondary inline-flex items-center gap-2"
              data-test="add-deepseek-channel"
              @click="addChannel"
            >
              <Icon name="plus" size="sm" />
              {{ t('admin.riskControl.addChannel') }}
            </button>
          </div>

          <p
            v-if="configForm.deepseek_channels.length === 0"
            class="border-y border-gray-200 py-8 text-center text-sm text-gray-500 dark:border-dark-700 dark:text-gray-400"
          >
            {{ t('admin.riskControl.noChannels') }}
          </p>

          <div v-else class="space-y-3">
            <article
              v-for="(channel, index) in configForm.deepseek_channels"
              :key="channel.id"
              class="rounded-lg border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-800"
              :data-test="`deepseek-channel-${index}`"
            >
              <div class="mb-4 flex flex-wrap items-center justify-between gap-3">
                <div class="flex min-w-0 items-center gap-3">
                  <span
                    class="flex h-8 min-w-8 items-center justify-center rounded-md bg-gray-100 px-2 text-xs font-semibold text-gray-600 dark:bg-dark-700 dark:text-gray-300"
                  >
                    {{ index + 1 }}
                  </span>
                  <div class="min-w-0">
                    <p class="truncate font-medium text-gray-900 dark:text-white">
                      {{ channel.name || t('admin.riskControl.unnamedChannel') }}
                    </p>
                    <p class="truncate text-xs text-gray-500 dark:text-gray-400">{{ channel.id }}</p>
                  </div>
                  <span class="rounded-md px-2 py-1 text-xs font-medium" :class="channelStatusClass(channel)">
                    {{ channelStatusLabel(channel) }}
                  </span>
                </div>
                <div class="flex items-center gap-1">
                  <Toggle v-model="channel.enabled" :data-test="`deepseek-channel-enabled-${index}`" />
                  <button
                    type="button"
                    class="btn btn-ghost btn-sm p-2"
                    :disabled="index === 0"
                    :title="t('admin.riskControl.moveChannelUp')"
                    :aria-label="t('admin.riskControl.moveChannelUp')"
                    :data-test="`deepseek-channel-move-up-${index}`"
                    @click="moveChannel(index, -1)"
                  >
                    <Icon name="arrowUp" size="sm" />
                  </button>
                  <button
                    type="button"
                    class="btn btn-ghost btn-sm p-2"
                    :disabled="index === configForm.deepseek_channels.length - 1"
                    :title="t('admin.riskControl.moveChannelDown')"
                    :aria-label="t('admin.riskControl.moveChannelDown')"
                    :data-test="`deepseek-channel-move-down-${index}`"
                    @click="moveChannel(index, 1)"
                  >
                    <Icon name="arrowDown" size="sm" />
                  </button>
                  <button
                    type="button"
                    class="btn btn-ghost btn-sm p-2 text-red-600 dark:text-red-300"
                    :title="t('admin.riskControl.removeChannel')"
                    :aria-label="t('admin.riskControl.removeChannel')"
                    @click="removeChannel(index)"
                  >
                    <Icon name="trash" size="sm" />
                  </button>
                </div>
              </div>

              <div class="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-12">
                <label class="xl:col-span-3">
                  <span class="input-label">{{ t('admin.riskControl.channelName') }}</span>
                  <input
                    v-model.trim="channel.name"
                    class="input"
                    type="text"
                    :data-test="`deepseek-channel-name-${index}`"
                  />
                </label>
                <label class="xl:col-span-4">
                  <span class="input-label">{{ t('admin.riskControl.channelBaseURL') }}</span>
                  <input
                    v-model.trim="channel.base_url"
                    class="input"
                    type="url"
                    placeholder="https://api.deepseek.com"
                    :data-test="`deepseek-channel-url-${index}`"
                  />
                </label>
                <label class="xl:col-span-3">
                  <span class="input-label">{{ t('admin.riskControl.channelModel') }}</span>
                  <input
                    v-model.trim="channel.model"
                    class="input"
                    type="text"
                    placeholder="deepseek-v4-flash"
                    :data-test="`deepseek-channel-model-${index}`"
                  />
                </label>
                <label class="xl:col-span-2">
                  <span class="input-label">{{ t('admin.riskControl.channelTimeout') }}</span>
                  <input
                    v-model.number="channel.timeout_ms"
                    class="input"
                    type="number"
                    min="100"
                    max="30000"
                    step="100"
                    :data-test="`deepseek-channel-timeout-${index}`"
                  />
                </label>
              </div>

              <div class="mt-4 grid grid-cols-1 gap-4 lg:grid-cols-[minmax(0,1fr)_auto] lg:items-end">
                <div>
                  <label :for="`channel-key-${channel.id}`" class="input-label">{{
                    t('admin.riskControl.channelAPIKey')
                  }}</label>
                  <div class="grid grid-cols-1 gap-2 sm:grid-cols-[minmax(0,1fr)_auto]">
                    <input
                      :id="`channel-key-${channel.id}`"
                      v-model="channel.api_key"
                      class="input font-mono"
                      type="password"
                      autocomplete="new-password"
                      :placeholder="
                        channel.api_key_configured
                          ? channel.api_key_masked
                          : t('admin.riskControl.channelAPIKeyPlaceholder')
                      "
                      :disabled="channel.clear_api_key"
                      :data-test="`deepseek-channel-key-${index}`"
                    />
                    <button
                      type="button"
                      class="btn btn-secondary inline-flex items-center justify-center gap-2"
                      :class="channel.clear_api_key ? 'border-red-300 text-red-700 dark:text-red-300' : ''"
                      :disabled="!channel.api_key_configured && !channel.clear_api_key"
                      @click="toggleClearChannelKey(channel)"
                    >
                      <Icon :name="channel.clear_api_key ? 'x' : 'trash'" size="sm" />
                      {{
                        channel.clear_api_key
                          ? t('admin.riskControl.keepChannelKey')
                          : t('admin.riskControl.clearChannelKey')
                      }}
                    </button>
                  </div>
                  <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
                    {{ channelKeyStatus(channel) }}
                  </p>
                </div>
                <button
                  type="button"
                  class="btn btn-secondary inline-flex min-w-32 items-center justify-center gap-2"
                  :disabled="!canTestChannel(channel) || testingChannelID !== null"
                  :data-test="`test-deepseek-channel-${index}`"
                  @click="testChannel(channel)"
                >
                  <Icon name="beaker" size="sm" :class="testingChannelID === channel.id ? 'animate-pulse' : ''" />
                  {{
                    testingChannelID === channel.id
                      ? t('admin.riskControl.testingChannel')
                      : t('admin.riskControl.testChannel')
                  }}
                </button>
              </div>

              <div class="mt-3 flex flex-wrap gap-x-4 gap-y-1 text-xs text-gray-500 dark:text-gray-400">
                <span>{{ t('admin.riskControl.channelBreaker') }}: {{ breakerLabel(channel) }}</span>
                <span>{{ t('admin.riskControl.channelLatency') }}: {{ latencyText(channel.last_latency_ms) }}</span>
                <span
                  >{{ t('admin.riskControl.channelHealthChecked') }}:
                  {{ dateOrDash(channel.last_health_checked_at) }}</span
                >
                <span v-if="channel.last_error" class="text-amber-700 dark:text-amber-300">{{
                  channel.last_error
                }}</span>
              </div>

              <div
                v-if="channelTestResults[channel.id]"
                class="mt-3 rounded-lg border px-3 py-2 text-sm"
                :class="channelTestResults[channel.id]?.health_valid ? statusClasses.healthy : statusClasses.warning"
                :data-test="`deepseek-channel-test-result-${index}`"
              >
                {{ channelTestResultText(channelTestResults[channel.id]!) }}
              </div>
            </article>
          </div>
        </section>

        <section class="border-y border-gray-200 py-6 dark:border-dark-700" aria-labelledby="policy-heading">
          <div class="grid grid-cols-1 gap-5 xl:grid-cols-[minmax(0,1fr)_180px_180px]">
            <div>
              <h2 id="policy-heading" class="text-lg font-semibold text-gray-900 dark:text-white">
                {{ t('admin.riskControl.policyTitle') }}
              </h2>
              <div class="mt-3 flex flex-wrap gap-2">
                <span
                  class="rounded-md bg-gray-100 px-2 py-1 font-mono text-xs text-gray-700 dark:bg-dark-700 dark:text-gray-300"
                >
                  {{ configForm.policy_version || '-' }}
                </span>
                <span
                  v-for="category in policyCategories"
                  :key="category"
                  class="rounded-md bg-emerald-50 px-2 py-1 text-xs text-emerald-800 dark:bg-emerald-900/20 dark:text-emerald-300"
                >
                  {{ t(`admin.riskControl.policyCategories.${category}`) }}
                </span>
              </div>
            </div>
            <label>
              <span class="input-label">{{ t('admin.riskControl.confidenceThreshold') }}</span>
              <input
                :value="percent(configForm.deepseek_threshold)"
                class="input bg-gray-50 dark:bg-dark-700"
                type="text"
                readonly
                data-test="deepseek-threshold"
              />
            </label>
            <label>
              <span class="input-label">{{ t('admin.riskControl.totalReviewBudget') }}</span>
              <input
                v-model.number="configForm.deepseek_total_timeout_ms"
                class="input"
                type="number"
                min="100"
                max="120000"
                step="100"
                data-test="deepseek-total-timeout"
              />
            </label>
          </div>
        </section>

        <section aria-labelledby="keywords-heading">
          <div class="mb-4 flex flex-col gap-1 sm:flex-row sm:items-end sm:justify-between">
            <div>
              <h2 id="keywords-heading" class="text-lg font-semibold text-gray-900 dark:text-white">
                {{ t('admin.riskControl.keywordTiersTitle') }}
              </h2>
              <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
                {{ t('admin.riskControl.keywordTiersSummary') }}
              </p>
            </div>
            <span class="text-sm text-gray-500 dark:text-gray-400">
              {{
                t('admin.riskControl.keywordCounts', { layer1: layer1Keywords.length, layer2: layer2Keywords.length })
              }}
            </span>
          </div>
          <div class="grid grid-cols-1 gap-5 lg:grid-cols-2">
            <label>
              <span class="input-label">{{ t('admin.riskControl.layer1Keywords') }}</span>
              <textarea
                v-model="configForm.layer1_keywords_text"
                class="input min-h-44 resize-y font-mono text-sm"
                data-test="layer1-keywords"
              ></textarea>
              <span class="mt-2 block text-xs text-gray-500 dark:text-gray-400">{{
                t('admin.riskControl.layer1KeywordsMeta')
              }}</span>
            </label>
            <label>
              <span class="input-label">{{ t('admin.riskControl.layer2Keywords') }}</span>
              <textarea
                v-model="configForm.layer2_keywords_text"
                class="input min-h-44 resize-y font-mono text-sm"
                data-test="layer2-keywords"
              ></textarea>
              <span class="mt-2 block text-xs text-gray-500 dark:text-gray-400">{{
                t('admin.riskControl.layer2KeywordsMeta')
              }}</span>
            </label>
          </div>
        </section>

        <details class="border-y border-gray-200 py-4 dark:border-dark-700" data-test="advanced-risk-settings">
          <summary
            class="flex cursor-pointer list-none items-center justify-between gap-3 font-medium text-gray-900 dark:text-white"
          >
            <span>{{ t('admin.riskControl.advancedSettings') }}</span>
            <Icon name="chevronDown" size="sm" />
          </summary>
          <div class="mt-5 grid grid-cols-1 gap-6 lg:grid-cols-3">
            <div class="space-y-4">
              <h3 class="font-medium text-gray-900 dark:text-white">{{ t('admin.riskControl.scopeTitle') }}</h3>
              <label class="flex items-center justify-between gap-3">
                <span class="text-sm text-gray-700 dark:text-gray-300">{{ t('admin.riskControl.allGroups') }}</span>
                <Toggle v-model="configForm.all_groups" />
              </label>
              <div
                v-if="!configForm.all_groups"
                class="max-h-44 space-y-2 overflow-y-auto rounded-lg border border-gray-200 p-3 dark:border-dark-700"
              >
                <label
                  v-for="group in groups"
                  :key="group.id"
                  class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300"
                >
                  <input
                    v-model="configForm.group_ids"
                    type="checkbox"
                    :value="group.id"
                    class="h-4 w-4 rounded border-gray-300 text-primary-600"
                  />
                  <span class="truncate">{{ group.name }}</span>
                </label>
              </div>
              <label>
                <span class="input-label">{{ t('admin.riskControl.userEmailWhitelist') }}</span>
                <textarea
                  v-model="configForm.user_email_whitelist_text"
                  class="input min-h-24 resize-y text-sm"
                  :placeholder="t('admin.riskControl.userEmailWhitelistPlaceholder')"
                ></textarea>
              </label>
              <label>
                <span class="input-label">{{ t('admin.riskControl.modelFilter') }}</span>
                <select v-model="configForm.model_filter_type" class="input">
                  <option value="all">{{ t('admin.riskControl.modelFilterAll') }}</option>
                  <option value="include">{{ t('admin.riskControl.modelFilterInclude') }}</option>
                  <option value="exclude">{{ t('admin.riskControl.modelFilterExclude') }}</option>
                </select>
              </label>
              <textarea
                v-if="configForm.model_filter_type !== 'all'"
                v-model="configForm.model_filter_models_text"
                class="input min-h-20 resize-y font-mono text-sm"
                :placeholder="t('admin.riskControl.modelFilterModels')"
              ></textarea>
            </div>

            <div class="space-y-4">
              <h3 class="font-medium text-gray-900 dark:text-white">{{ t('admin.riskControl.responseTitle') }}</h3>
              <label>
                <span class="input-label">{{ t('admin.riskControl.blockStatus') }}</span>
                <input v-model.number="configForm.block_status" class="input" type="number" min="400" max="599" />
              </label>
              <label>
                <span class="input-label">{{ t('admin.riskControl.blockMessage') }}</span>
                <input v-model.trim="configForm.block_message" class="input" type="text" />
              </label>
              <label class="flex items-center justify-between gap-3">
                <span class="text-sm text-gray-700 dark:text-gray-300">{{ t('admin.riskControl.emailOnHit') }}</span>
                <Toggle v-model="configForm.email_on_hit" />
              </label>
              <label class="flex items-center justify-between gap-3">
                <span class="text-sm text-gray-700 dark:text-gray-300">{{ t('admin.riskControl.autoBan') }}</span>
                <Toggle v-model="configForm.auto_ban_enabled" />
              </label>
              <label class="flex items-center justify-between gap-3">
                <span class="min-w-0">
                  <span class="block text-sm text-gray-700 dark:text-gray-300">{{
                    t('admin.riskControl.cyberPolicyExcludeBan')
                  }}</span>
                  <span class="mt-1 block text-xs text-gray-500 dark:text-gray-400">{{
                    t('admin.riskControl.cyberPolicyExcludeBanHint')
                  }}</span>
                </span>
                <Toggle
                  v-model="configForm.cyber_policy_exclude_from_ban_count"
                  class="flex-shrink-0"
                  data-test="cyber-policy-exclude-ban"
                />
              </label>
              <label>
                <span class="input-label">{{ t('admin.riskControl.banThreshold') }}</span>
                <input v-model.number="configForm.ban_threshold" class="input" type="number" min="1" max="1000" />
              </label>
            </div>

            <div class="space-y-4">
              <h3 class="font-medium text-gray-900 dark:text-white">{{ t('admin.riskControl.retentionTitle') }}</h3>
              <label class="flex items-center justify-between gap-3">
                <span class="text-sm text-gray-700 dark:text-gray-300">{{ t('admin.riskControl.recordNonHits') }}</span>
                <Toggle v-model="configForm.record_non_hits" />
              </label>
              <label>
                <span class="input-label">{{ t('admin.riskControl.hitRetentionDays') }}</span>
                <input v-model.number="configForm.hit_retention_days" class="input" type="number" min="1" max="3650" />
              </label>
              <label>
                <span class="input-label">{{ t('admin.riskControl.nonHitRetentionDays') }}</span>
                <input v-model.number="configForm.non_hit_retention_days" class="input" type="number" min="1" max="3" />
              </label>
              <label>
                <span class="input-label">{{ t('admin.riskControl.violationWindowHours') }}</span>
                <input
                  v-model.number="configForm.violation_window_hours"
                  class="input"
                  type="number"
                  min="1"
                  max="8760"
                />
              </label>
            </div>
          </div>
        </details>

        <section
          class="overflow-hidden rounded-lg border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-800"
          aria-labelledby="records-heading"
        >
          <div class="border-b border-gray-200 px-4 py-4 dark:border-dark-700 sm:px-5">
            <div class="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
              <div>
                <h2 id="records-heading" class="text-lg font-semibold text-gray-900 dark:text-white">
                  {{ t('admin.riskControl.records') }}
                </h2>
                <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.riskControl.recordsSummary') }}</p>
              </div>
              <div class="grid grid-cols-1 gap-2 sm:grid-cols-3">
                <input
                  v-model.trim="filters.search"
                  class="input"
                  type="search"
                  :placeholder="t('admin.riskControl.filters.search')"
                  @keyup.enter="reloadLogs"
                />
                <input
                  v-model="filters.from"
                  class="input"
                  type="datetime-local"
                  :title="t('admin.riskControl.filters.from')"
                  @change="reloadLogs"
                />
                <input
                  v-model="filters.to"
                  class="input"
                  type="datetime-local"
                  :title="t('admin.riskControl.filters.to')"
                  @change="reloadLogs"
                />
              </div>
            </div>

            <nav
              class="mt-4 grid grid-cols-2 gap-1 rounded-lg bg-gray-100 p-1 dark:bg-dark-900 sm:grid-cols-4"
              role="tablist"
              :aria-label="t('admin.riskControl.records')"
            >
              <button
                v-for="tab in recordTabs"
                :key="tab.value"
                type="button"
                role="tab"
                class="min-h-10 rounded-md px-3 py-2 text-sm font-medium transition-colors"
                :class="
                  filters.result === tab.value
                    ? 'bg-white text-gray-900 shadow-sm dark:bg-dark-700 dark:text-white'
                    : 'text-gray-500 hover:text-gray-900 dark:text-gray-400 dark:hover:text-white'
                "
                :aria-selected="filters.result === tab.value"
                :data-test="`record-tab-${tab.value}`"
                @click="selectLogView(tab.value)"
              >
                {{ tab.label }}
              </button>
            </nav>
          </div>

          <div class="overflow-x-auto" data-test="audit-log-table">
            <table class="min-w-[1120px] w-full divide-y divide-gray-200 dark:divide-dark-700">
              <thead class="bg-gray-50 dark:bg-dark-900/40">
                <tr>
                  <th
                    v-for="column in logColumns"
                    :key="column"
                    class="px-4 py-3 text-left text-xs font-medium uppercase text-gray-500 dark:text-gray-400"
                  >
                    {{ t(`admin.riskControl.table.${column}`) }}
                  </th>
                </tr>
              </thead>
              <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                <tr v-if="logsLoading">
                  <td
                    :colspan="logColumns.length"
                    class="px-4 py-12 text-center text-sm text-gray-500 dark:text-gray-400"
                  >
                    {{ t('common.loading') }}
                  </td>
                </tr>
                <tr v-else-if="logs.length === 0">
                  <td
                    :colspan="logColumns.length"
                    class="px-4 py-12 text-center text-sm text-gray-500 dark:text-gray-400"
                  >
                    {{ t('admin.riskControl.emptyLogs') }}
                  </td>
                </tr>
                <tr v-for="row in logs" v-else :key="row.id" class="hover:bg-gray-50 dark:hover:bg-dark-700/50">
                  <td class="whitespace-nowrap px-4 py-3 text-sm text-gray-600 dark:text-gray-300">
                    {{ formatDateTime(row.created_at) }}
                  </td>
                  <td class="px-4 py-3 text-sm text-gray-700 dark:text-gray-300">
                    <div class="max-w-48 truncate">{{ row.user_email || '-' }}</div>
                    <div class="text-xs text-gray-400">{{ row.group_name || '-' }}</div>
                  </td>
                  <td class="px-4 py-3 text-sm text-gray-700 dark:text-gray-300">
                    <div class="max-w-48 truncate">{{ row.model || '-' }}</div>
                    <div class="text-xs text-gray-400">{{ row.keyword_tier || row.context_class || '-' }}</div>
                  </td>
                  <td class="px-4 py-3">
                    <span class="inline-flex rounded-md px-2 py-1 text-xs font-medium" :class="resultClass(row)">{{
                      resultLabel(row)
                    }}</span>
                    <span
                      v-if="row.reviewer_disagreement"
                      class="ml-1 inline-flex rounded-md bg-amber-50 px-2 py-1 text-xs font-medium text-amber-700 dark:bg-amber-900/20 dark:text-amber-300"
                    >
                      {{ t('admin.riskControl.reviewerDisagreement') }}
                    </span>
                  </td>
                  <td class="px-4 py-3 text-sm text-gray-700 dark:text-gray-300">
                    <div>{{ row.deepseek_category || row.highest_category || '-' }}</div>
                    <div class="text-xs text-gray-400" data-test="review-confidence">{{ confidenceText(row) }}</div>
                    <div
                      v-if="row.deepseek_reason"
                      class="mt-1 max-w-48 truncate text-xs text-gray-500 dark:text-gray-400"
                    >
                      {{ row.deepseek_reason }}
                    </div>
                  </td>
                  <td class="px-4 py-3 text-sm text-gray-700 dark:text-gray-300">
                    <div>{{ firstAttemptName(row) }}</div>
                    <div class="text-xs text-gray-400">{{ attemptSummary(row) }}</div>
                  </td>
                  <td class="whitespace-nowrap px-4 py-3 text-sm text-gray-700 dark:text-gray-300">
                    {{ latencyText(row.upstream_latency_ms) }}
                  </td>
                  <td class="w-72 px-4 py-3 text-sm text-gray-700 dark:text-gray-300">
                    <button
                      type="button"
                      class="group flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left hover:bg-gray-100 dark:hover:bg-dark-700"
                      @click="openLog(row)"
                    >
                      <span class="min-w-0 flex-1 truncate">{{ row.input_excerpt || row.error || '-' }}</span>
                      <Icon name="eye" size="xs" class="flex-shrink-0 text-gray-400" />
                    </button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <Pagination
            v-if="pagination.total > 0"
            :page="pagination.page"
            :page-size="pagination.page_size"
            :total="pagination.total"
            @update:page="changePage"
            @update:page-size="changePageSize"
          />
        </section>
      </template>

      <BaseDialog
        :show="selectedLog !== null"
        :title="t('admin.riskControl.inputDetailTitle')"
        width="extra-wide"
        @close="selectedLog = null"
      >
        <div v-if="selectedLog" class="space-y-5" data-test="audit-log-detail">
          <dl class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <div v-for="item in selectedLogMeta" :key="item.label" class="min-w-0">
              <dt class="text-xs font-medium text-gray-500 dark:text-gray-400">{{ item.label }}</dt>
              <dd class="mt-1 break-words text-sm text-gray-900 dark:text-white">{{ item.value }}</dd>
            </div>
          </dl>

          <div v-if="selectedLog.review_attempts?.length" data-test="review-attempts">
            <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
              {{ t('admin.riskControl.reviewAttempts') }}
            </h3>
            <div
              class="mt-2 divide-y divide-gray-100 rounded-lg border border-gray-200 dark:divide-dark-700 dark:border-dark-700"
            >
              <div
                v-for="(attempt, index) in selectedLog.review_attempts"
                :key="`${attempt.channel_id}-${index}`"
                class="grid grid-cols-1 gap-1 px-3 py-2 text-sm sm:grid-cols-[minmax(0,1fr)_120px_100px]"
              >
                <span class="truncate text-gray-900 dark:text-white">{{
                  attempt.channel_name || attempt.channel_id || attempt.reviewer || '-'
                }}</span>
                <span class="text-gray-500 dark:text-gray-400">{{
                  attempt.outcome || (attempt.http_status ? `HTTP ${attempt.http_status}` : '-')
                }}</span>
                <span class="text-gray-500 dark:text-gray-400">{{ latencyText(attempt.latency_ms) }}</span>
                <span v-if="attempt.error" class="text-xs text-amber-700 dark:text-amber-300 sm:col-span-3">{{
                  attempt.error
                }}</span>
              </div>
            </div>
          </div>

          <div v-if="selectedLog.evidence_windows?.length" class="space-y-3" data-test="evidence-windows">
            <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
              {{ t('admin.riskControl.inputDetailContent') }}
            </h3>
            <div
              v-for="(window, index) in selectedLog.evidence_windows"
              :key="`${window.path}-${index}`"
              class="rounded-lg border border-gray-200 p-3 dark:border-dark-700"
              data-test="evidence-window"
            >
              <div class="flex flex-wrap gap-2 text-xs text-gray-500 dark:text-gray-400">
                <span>{{ window.context_class }}</span>
                <span class="font-mono">{{ window.path }}</span>
              </div>
              <pre
                class="mt-2 max-h-52 overflow-auto whitespace-pre-wrap break-words rounded-md bg-gray-50 p-3 text-xs text-gray-700 dark:bg-dark-900/50 dark:text-gray-300"
                >{{ window.text }}</pre
              >
              <div v-if="window.matches?.length" class="mt-2 flex flex-wrap gap-1">
                <span
                  v-for="match in window.matches"
                  :key="`${match.rule_id}-${match.start}`"
                  class="rounded bg-amber-50 px-2 py-1 text-xs text-amber-800 dark:bg-amber-900/20 dark:text-amber-300"
                >
                  {{ match.keyword }} · {{ match.tier }}
                </span>
              </div>
            </div>
          </div>

          <div v-else class="rounded-lg bg-gray-50 p-4 text-sm text-gray-700 dark:bg-dark-900/40 dark:text-gray-300">
            {{ selectedLog.input_excerpt || selectedLog.error || '-' }}
          </div>
        </div>
        <template #footer>
          <button type="button" class="btn btn-secondary" @click="selectedLog = null">{{ t('common.close') }}</button>
        </template>
      </BaseDialog>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import Toggle from '@/components/common/Toggle.vue'
import Pagination from '@/components/common/Pagination.vue'
import { adminAPI } from '@/api/admin'
import type {
  ContentModerationConfig,
  ContentModerationFirstLayerStage,
  ContentModerationLayerStage,
  ContentModerationLog,
  ContentModerationLogView,
  ContentModerationModelFilterType,
  ContentModerationRuntimeStatus,
  DeepSeekModerationChannel,
  TestDeepSeekChannelResponse,
  UpdateContentModerationConfig,
} from '@/api/admin/riskControl'
import type { AdminGroup } from '@/types'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatDateTime as formatDateTimeValue } from '@/utils/format'

type EditableDeepSeekChannel = DeepSeekModerationChannel & {
  api_key: string
  clear_api_key: boolean
}

type LayerID = 'layer1' | 'layer2'
type OverviewIcon = 'shield' | 'cloud' | 'swap' | 'document' | 'database'

interface OverviewCacheMetrics {
  hits: number
  misses: number
  writes: number
  errors: number
}

interface OverviewItem {
  key: string
  label: string
  value: string
  meta: string
  icon: OverviewIcon
  iconClass: string
  cacheMetrics?: OverviewCacheMetrics
}

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(true)
const refreshing = ref(false)
const saving = ref(false)
const logsLoading = ref(false)
const testingChannelID = ref<string | null>(null)
const status = ref<ContentModerationRuntimeStatus | null>(null)
const groups = ref<AdminGroup[]>([])
const logs = ref<ContentModerationLog[]>([])
const selectedLog = ref<ContentModerationLog | null>(null)
const channelTestResults = reactive<Record<string, TestDeepSeekChannelResponse | undefined>>({})
const savedChannelDigests = reactive<Record<string, string | undefined>>({})
let refreshTimer: number | null = null

const defaultBlockMessage = () => t('admin.riskControl.defaultBlockMessage')

const configForm = reactive({
  enabled: true,
  mode: 'pre_block' as const,
  deepseek_enabled: true,
  yufeng_enabled: false,
  deepseek_total_timeout_ms: 10000,
  deepseek_threshold: 0.8,
  policy_version: '',
  deepseek_channels: [] as EditableDeepSeekChannel[],
  all_groups: true,
  group_ids: [] as number[],
  user_email_whitelist_text: '',
  record_non_hits: false,
  block_status: 403,
  block_message: defaultBlockMessage(),
  email_on_hit: true,
  auto_ban_enabled: true,
  cyber_policy_exclude_from_ban_count: false,
  ban_threshold: 10,
  violation_window_hours: 720,
  hit_retention_days: 180,
  non_hit_retention_days: 3,
  model_filter_type: 'all' as ContentModerationModelFilterType,
  model_filter_models_text: '',
  first_layer_stage: 'shadow' as ContentModerationFirstLayerStage,
  second_layer_stage: 'shadow' as ContentModerationLayerStage,
  layer1_keywords_text: '',
  layer2_keywords_text: '',
})

const pagination = reactive({ page: 1, page_size: 20, total: 0, pages: 1 })
const filters = reactive({
  result: 'risky_shadow' as ContentModerationLogView,
  search: '',
  from: '',
  to: '',
})

const statusClasses = {
  healthy: 'bg-emerald-50 text-emerald-800 dark:bg-emerald-900/20 dark:text-emerald-300',
  warning: 'bg-amber-50 text-amber-800 dark:bg-amber-900/20 dark:text-amber-300',
  danger: 'bg-red-50 text-red-800 dark:bg-red-900/20 dark:text-red-300',
  unknown: 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300',
}

const policyCategories = ['cyber', 'accountAbuse', 'deepfakeDoxThreat', 'selfHarm', 'weapons', 'sexualContent']

const logColumns = ['time', 'user', 'request', 'decision', 'modelResult', 'channelAttempts', 'latency', 'input']

const recordTabs = computed<Array<{ value: ContentModerationLogView; label: string }>>(() => [
  { value: 'risky_shadow', label: t('admin.riskControl.recordTabs.riskyShadow') },
  { value: 'content_blocked', label: t('admin.riskControl.recordTabs.blocked') },
  { value: 'review_unavailable', label: t('admin.riskControl.recordTabs.reviewUnavailable') },
  { value: 'cyber_policy', label: t('admin.riskControl.recordTabs.cyberPolicy') },
])

const stageOptions = computed(() => [
  {
    value: 'shadow' as const,
    label: t('admin.riskControl.stageShadow'),
    activeClass: 'bg-amber-100 text-amber-800 shadow-sm dark:bg-amber-900/30 dark:text-amber-300',
  },
  {
    value: 'enforce' as const,
    label: t('admin.riskControl.stageEnforce'),
    activeClass: 'bg-red-100 text-red-800 shadow-sm dark:bg-red-900/30 dark:text-red-300',
  },
])

const layerRows = computed(() => [
  {
    id: 'layer1' as const,
    label: t('admin.riskControl.layer1Title'),
    meta: t('admin.riskControl.layer1Meta'),
    stage: configForm.first_layer_stage,
  },
  {
    id: 'layer2' as const,
    label: t('admin.riskControl.layer2Title'),
    meta: t('admin.riskControl.layer2Meta'),
    stage: configForm.second_layer_stage,
  },
])

const enabledChannelCount = computed(() => configForm.deepseek_channels.filter((channel) => channel.enabled).length)
const healthyChannelCount = computed(
  () => configForm.deepseek_channels.filter((channel) => channel.enabled && isChannelHealthy(channel)).length
)

const secondLayerEnforceReady = computed(() => {
  if (status.value?.second_layer_enforce_ready !== undefined) {
    return status.value.second_layer_enforce_ready
  }
  if (!configForm.deepseek_enabled && !configForm.yufeng_enabled) return false
  if (configForm.yufeng_enabled) return false
  return configForm.deepseek_enabled && healthyChannelCount.value > 0
})

const enforceGateText = computed(() => {
  if (secondLayerEnforceReady.value) return t('admin.riskControl.enforceGateReady')
  return status.value?.second_layer_enforce_reason || t('admin.riskControl.enforceGateBlocked')
})

const layer1Keywords = computed(() => parseLineList(configForm.layer1_keywords_text))
const layer2Keywords = computed(() => parseLineList(configForm.layer2_keywords_text))

const overviewItems = computed<OverviewItem[]>(() => [
  {
    key: 'runtime',
    label: t('admin.riskControl.overview.status'),
    value: configForm.enabled ? t('admin.riskControl.overview.enabled') : t('admin.riskControl.overview.disabled'),
    meta: t('admin.riskControl.overview.reviewed', { count: status.value?.pre_block_checked ?? 0 }),
    icon: 'shield',
    iconClass: configForm.enabled
      ? 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/20 dark:text-emerald-300'
      : 'bg-gray-100 text-gray-500 dark:bg-dark-700 dark:text-gray-400',
  },
  {
    key: 'deepseek',
    label: 'DeepSeek',
    value: configForm.deepseek_enabled
      ? t('admin.riskControl.overview.enabled')
      : t('admin.riskControl.overview.disabled'),
    meta: t('admin.riskControl.channelHealthSummary', {
      healthy: healthyChannelCount.value,
      enabled: enabledChannelCount.value,
    }),
    icon: 'cloud',
    iconClass: 'bg-sky-50 text-sky-700 dark:bg-sky-900/20 dark:text-sky-300',
  },
  {
    key: 'failover',
    label: t('admin.riskControl.overview.failover'),
    value: String(status.value?.deepseek_failover_count ?? 0),
    meta: t('admin.riskControl.overview.unavailable', { count: status.value?.deepseek_unavailable_count ?? 0 }),
    icon: 'swap',
    iconClass: 'bg-amber-50 text-amber-700 dark:bg-amber-900/20 dark:text-amber-300',
  },
  {
    key: 'second-layer-cache',
    label: t('admin.riskControl.overview.secondLayerCache'),
    value: t('admin.riskControl.overview.cacheHits', {
      count: status.value?.second_layer_cache_hits ?? 0,
    }),
    meta: t('admin.riskControl.overview.cacheActivity', {
      misses: status.value?.second_layer_cache_misses ?? 0,
      writes: status.value?.second_layer_cache_writes ?? 0,
      errors: status.value?.second_layer_cache_errors ?? 0,
    }),
    icon: 'database',
    iconClass:
      (status.value?.second_layer_cache_errors ?? 0) > 0
        ? 'bg-red-50 text-red-700 dark:bg-red-900/20 dark:text-red-300'
        : 'bg-indigo-50 text-indigo-700 dark:bg-indigo-900/20 dark:text-indigo-300',
    cacheMetrics: {
      hits: status.value?.second_layer_cache_hits ?? 0,
      misses: status.value?.second_layer_cache_misses ?? 0,
      writes: status.value?.second_layer_cache_writes ?? 0,
      errors: status.value?.second_layer_cache_errors ?? 0,
    },
  },
  {
    key: 'logs',
    label: t('admin.riskControl.overview.logs'),
    value: String(pagination.total),
    meta: t('admin.riskControl.overview.currentFilter'),
    icon: 'document',
    iconClass: 'bg-violet-50 text-violet-700 dark:bg-violet-900/20 dark:text-violet-300',
  },
])

const selectedLogMeta = computed(() => {
  const row = selectedLog.value
  if (!row) return []
  return [
    { label: t('admin.riskControl.auditRequestID'), value: row.request_id || '-' },
    { label: t('admin.riskControl.auditDecisionSource'), value: row.decision_source || '-' },
    {
      label: t('admin.riskControl.table.modelResult'),
      value: `${row.deepseek_category || row.highest_category || '-'} / ${confidenceText(row)}`,
    },
    { label: t('admin.riskControl.reviewOutcome'), value: row.review_outcome || row.action || '-' },
  ]
})

function defaultChannel(): EditableDeepSeekChannel {
  return {
    id: 'deepseek-official',
    name: t('admin.riskControl.officialChannelName'),
    base_url: 'https://api.deepseek.com',
    model: 'deepseek-v4-flash',
    enabled: true,
    order: 0,
    timeout_ms: 3000,
    api_key_configured: false,
    api_key_masked: '',
    health_status: 'unknown',
    breaker_status: 'unknown',
    api_key: '',
    clear_api_key: false,
  }
}

function editableChannel(channel: DeepSeekModerationChannel, index: number): EditableDeepSeekChannel {
  return {
    ...channel,
    order: Number.isFinite(channel.order) ? channel.order : index,
    model: channel.model || 'deepseek-v4-flash',
    timeout_ms: channel.timeout_ms || 3000,
    api_key_configured: Boolean(channel.api_key_configured),
    api_key_masked: channel.api_key_masked || '',
    api_key: '',
    clear_api_key: false,
  }
}

function applyConfig(config: ContentModerationConfig) {
  configForm.enabled = config.enabled ?? true
  configForm.deepseek_enabled = config.deepseek_enabled ?? true
  configForm.yufeng_enabled = config.yufeng_enabled ?? false
  configForm.deepseek_total_timeout_ms = config.deepseek_total_timeout_ms ?? 10000
  configForm.deepseek_threshold = config.deepseek_threshold ?? 0.8
  configForm.policy_version = config.policy_version || config.keyword_policy_version || ''
  const channels = config.deepseek_channels === undefined ? [defaultChannel()] : config.deepseek_channels
  configForm.deepseek_channels = [...channels]
    .sort((left, right) => (left.order ?? 0) - (right.order ?? 0))
    .map(editableChannel)
  for (const id of Object.keys(savedChannelDigests)) delete savedChannelDigests[id]
  for (const channel of configForm.deepseek_channels) {
    savedChannelDigests[channel.id] = channelDigest(channel)
  }
  configForm.all_groups = config.all_groups ?? true
  configForm.group_ids = [...(config.group_ids ?? [])]
  configForm.user_email_whitelist_text = (config.user_email_whitelist ?? []).join('\n')
  configForm.record_non_hits = config.record_non_hits ?? false
  configForm.block_status = config.block_status ?? 403
  configForm.block_message = config.block_message || defaultBlockMessage()
  configForm.email_on_hit = config.email_on_hit ?? true
  configForm.auto_ban_enabled = config.auto_ban_enabled ?? true
  configForm.cyber_policy_exclude_from_ban_count = config.cyber_policy_exclude_from_ban_count ?? false
  configForm.ban_threshold = config.ban_threshold ?? 10
  configForm.violation_window_hours = config.violation_window_hours ?? 720
  configForm.hit_retention_days = config.hit_retention_days ?? 180
  configForm.non_hit_retention_days = config.non_hit_retention_days ?? 3
  configForm.model_filter_type = config.model_filter?.type ?? 'all'
  configForm.model_filter_models_text = (config.model_filter?.models ?? []).join('\n')
  configForm.first_layer_stage = config.first_layer_stage === 'enforce' ? 'enforce' : 'shadow'
  configForm.second_layer_stage = config.second_layer_stage === 'enforce' ? 'enforce' : 'shadow'
  configForm.layer1_keywords_text = (config.layer1_keywords ?? []).join('\n')
  configForm.layer2_keywords_text = (config.layer2_keywords ?? []).join('\n')
}

async function loadInitial() {
  loading.value = true
  try {
    const [config, availableGroups] = await Promise.all([
      adminAPI.riskControl.getConfig(),
      adminAPI.groups.getAll().catch(() => [] as AdminGroup[]),
    ])
    applyConfig(config)
    groups.value = availableGroups
    await Promise.all([loadStatus(true), loadLogs()])
  } catch (error: unknown) {
    appStore.showError(extractApiErrorMessage(error, t('admin.riskControl.loadFailed')))
  } finally {
    loading.value = false
  }
}

async function refreshAll() {
  if (refreshing.value) return
  refreshing.value = true
  try {
    const config = await adminAPI.riskControl.getConfig()
    applyConfig(config)
    await Promise.all([loadStatus(false), loadLogs()])
  } catch (error: unknown) {
    appStore.showError(extractApiErrorMessage(error, t('admin.riskControl.loadFailed')))
  } finally {
    refreshing.value = false
  }
}

async function loadStatus(silent: boolean) {
  try {
    status.value = await adminAPI.riskControl.getStatus()
  } catch (error: unknown) {
    if (!silent) appStore.showError(extractApiErrorMessage(error, t('admin.riskControl.statusFailed')))
  }
}

async function loadLogs() {
  logsLoading.value = true
  try {
    const result = await adminAPI.riskControl.listLogs({
      page: pagination.page,
      page_size: pagination.page_size,
      result: filters.result,
      search: filters.search || undefined,
      from: normalizeLocalDateTime(filters.from),
      to: normalizeLocalDateTime(filters.to),
    })
    logs.value = result.items
    pagination.total = result.total
    pagination.page = result.page
    pagination.page_size = result.page_size
    pagination.pages = result.pages
  } catch (error: unknown) {
    appStore.showError(extractApiErrorMessage(error, t('admin.riskControl.logsFailed')))
  } finally {
    logsLoading.value = false
  }
}

async function saveConfig() {
  if (saving.value || !validateConfig()) return
  saving.value = true
  try {
    const payload: UpdateContentModerationConfig = {
      enabled: configForm.enabled,
      mode: 'pre_block',
      deepseek_enabled: configForm.deepseek_enabled,
      yufeng_enabled: configForm.yufeng_enabled,
      deepseek_total_timeout_ms: clampInteger(configForm.deepseek_total_timeout_ms, 100, 120000, 10000),
      deepseek_threshold: 0.8,
      deepseek_channels: configForm.deepseek_channels.map((channel, index) => ({
        id: channel.id.trim(),
        name: channel.name.trim(),
        base_url: channel.base_url.trim().replace(/\/$/, ''),
        model: channel.model.trim() || 'deepseek-v4-flash',
        enabled: channel.enabled,
        order: index,
        timeout_ms: clampInteger(channel.timeout_ms, 100, 30000, 3000),
        api_key: channel.api_key.trim() || undefined,
        clear_api_key: channel.clear_api_key || undefined,
      })),
      first_layer_stage: configForm.first_layer_stage,
      second_layer_enabled: configForm.deepseek_enabled || configForm.yufeng_enabled,
      second_layer_stage: configForm.second_layer_stage,
      layer1_keywords: layer1Keywords.value,
      layer2_keywords: layer2Keywords.value,
      all_groups: configForm.all_groups,
      group_ids: configForm.all_groups ? [] : [...configForm.group_ids],
      user_email_whitelist: parseEmailList(configForm.user_email_whitelist_text),
      record_non_hits: configForm.record_non_hits,
      block_status: clampInteger(configForm.block_status, 400, 599, 403),
      block_message: configForm.block_message.trim() || defaultBlockMessage(),
      email_on_hit: configForm.email_on_hit,
      auto_ban_enabled: configForm.auto_ban_enabled,
      cyber_policy_exclude_from_ban_count: configForm.cyber_policy_exclude_from_ban_count,
      ban_threshold: clampInteger(configForm.ban_threshold, 1, 1000, 10),
      violation_window_hours: clampInteger(configForm.violation_window_hours, 1, 8760, 720),
      hit_retention_days: clampInteger(configForm.hit_retention_days, 1, 3650, 180),
      non_hit_retention_days: clampInteger(configForm.non_hit_retention_days, 1, 3, 3),
      model_filter: {
        type: configForm.model_filter_type,
        models: configForm.model_filter_type === 'all' ? [] : parseLineList(configForm.model_filter_models_text),
      },
    }
    const updated = await adminAPI.riskControl.updateConfig(payload)
    applyConfig(updated)
    await Promise.all([loadStatus(true), loadLogs()])
    appStore.showSuccess(t('admin.riskControl.saved'))
  } catch (error: unknown) {
    appStore.showError(extractApiErrorMessage(error, t('admin.riskControl.saveFailed')))
  } finally {
    saving.value = false
  }
}

function validateConfig(): boolean {
  if (configForm.deepseek_enabled && enabledChannelCount.value === 0) {
    appStore.showError(t('admin.riskControl.enabledChannelRequired'))
    return false
  }
  const ids = new Set<string>()
  for (const channel of configForm.deepseek_channels) {
    const id = channel.id.trim()
    if (!isValidChannelID(id) || !channel.name.trim() || !channel.model.trim() || !isAllowedBaseURL(channel.base_url)) {
      appStore.showError(t('admin.riskControl.invalidChannel'))
      return false
    }
    if (ids.has(id)) {
      appStore.showError(t('admin.riskControl.duplicateChannelID'))
      return false
    }
    ids.add(id)
  }
  if (configForm.second_layer_stage === 'enforce' && !secondLayerEnforceReady.value) {
    appStore.showError(t('admin.riskControl.enforceGateBlocked'))
    return false
  }
  if (configForm.model_filter_type !== 'all' && parseLineList(configForm.model_filter_models_text).length === 0) {
    appStore.showError(t('admin.riskControl.modelFilterModelsRequired'))
    return false
  }
  return true
}

function addChannel() {
  const index = configForm.deepseek_channels.length
  configForm.deepseek_channels.push({
    ...defaultChannel(),
    id: `deepseek-channel-${Date.now()}`,
    name: t('admin.riskControl.newChannelName', { number: index + 1 }),
    order: index,
    enabled: false,
  })
}

function removeChannel(index: number) {
  const channel = configForm.deepseek_channels[index]
  if (!channel) return
  if (channel.api_key_configured && !window.confirm(t('admin.riskControl.removeChannelConfirm'))) return
  configForm.deepseek_channels.splice(index, 1)
  normalizeChannelOrder()
}

function moveChannel(index: number, direction: -1 | 1) {
  const target = index + direction
  if (target < 0 || target >= configForm.deepseek_channels.length) return
  const [channel] = configForm.deepseek_channels.splice(index, 1)
  configForm.deepseek_channels.splice(target, 0, channel)
  normalizeChannelOrder()
}

function normalizeChannelOrder() {
  configForm.deepseek_channels.forEach((channel, index) => {
    channel.order = index
  })
}

function toggleClearChannelKey(channel: EditableDeepSeekChannel) {
  channel.clear_api_key = !channel.clear_api_key
  if (channel.clear_api_key) channel.api_key = ''
}

function canTestChannel(channel: EditableDeepSeekChannel): boolean {
  return channel.api_key_configured && isChannelPersisted(channel)
}

async function testChannel(channel: EditableDeepSeekChannel) {
  if (!canTestChannel(channel) || testingChannelID.value !== null) return
  testingChannelID.value = channel.id
  try {
    channelTestResults[channel.id] = await adminAPI.riskControl.testDeepSeekChannel(channel.id)
    const config = await adminAPI.riskControl.getConfig()
    applyConfig(config)
    await loadStatus(true)
    appStore.showSuccess(t('admin.riskControl.channelTestComplete'))
  } catch (error: unknown) {
    appStore.showError(extractApiErrorMessage(error, t('admin.riskControl.channelTestFailed')))
  } finally {
    testingChannelID.value = null
  }
}

function setLayerStage(layer: LayerID, stage: ContentModerationLayerStage) {
  if (layer === 'layer2' && stage === 'enforce' && !secondLayerEnforceReady.value) return
  if (layer === 'layer1') configForm.first_layer_stage = stage
  else configForm.second_layer_stage = stage
}

function selectLogView(view: ContentModerationLogView) {
  if (filters.result === view) return
  filters.result = view
  reloadLogs()
}

function reloadLogs() {
  pagination.page = 1
  void loadLogs()
}

function changePage(page: number) {
  pagination.page = page
  void loadLogs()
}

function changePageSize(pageSize: number) {
  pagination.page = 1
  pagination.page_size = pageSize
  void loadLogs()
}

function openLog(row: ContentModerationLog) {
  selectedLog.value = row
}

function channelKeyStatus(channel: EditableDeepSeekChannel): string {
  if (channel.clear_api_key) return t('admin.riskControl.channelKeyWillClear')
  if (channel.api_key.trim()) return t('admin.riskControl.channelKeyWillReplace')
  if (channel.api_key_configured)
    return t('admin.riskControl.channelKeyStored', { masked: channel.api_key_masked || '***' })
  return t('admin.riskControl.channelKeyMissing')
}

function isChannelHealthy(channel: EditableDeepSeekChannel): boolean {
  if (!isChannelPersisted(channel)) return false
  if (channel.health_status === 'healthy' || channel.health_status === 'valid') return true
  if (!channel.healthy_until) return false
  return new Date(channel.healthy_until).getTime() > Date.now()
}

function channelStatusLabel(channel: EditableDeepSeekChannel): string {
  if (!channel.enabled) return t('admin.riskControl.channelDisabled')
  if (!isChannelPersisted(channel)) return t('admin.riskControl.channelNeedsSave')
  if (isChannelHealthy(channel)) return t('admin.riskControl.channelHealthy')
  if (isBreakerUnavailable(channel.breaker_status)) return t('admin.riskControl.channelCircuitOpen')
  return t('admin.riskControl.channelNeedsTest')
}

function channelStatusClass(channel: EditableDeepSeekChannel): string {
  if (!channel.enabled) return statusClasses.unknown
  if (!isChannelPersisted(channel)) return statusClasses.warning
  if (isChannelHealthy(channel)) return statusClasses.healthy
  if (isBreakerUnavailable(channel.breaker_status)) return statusClasses.danger
  return statusClasses.warning
}

function channelDigest(channel: EditableDeepSeekChannel): string {
  return JSON.stringify({
    id: channel.id.trim(),
    base_url: channel.base_url.trim().replace(/\/$/, ''),
    model: channel.model.trim(),
    timeout_ms: Number(channel.timeout_ms),
  })
}

function isBreakerUnavailable(state?: string): boolean {
  return state === 'open' || state === 'disabled' || state === 'auth_disabled' || state === 'cooldown'
}

function isChannelPersisted(channel: EditableDeepSeekChannel): boolean {
  return (
    channel.api_key.trim() === '' &&
    !channel.clear_api_key &&
    savedChannelDigests[channel.id] === channelDigest(channel)
  )
}

function breakerLabel(channel: EditableDeepSeekChannel): string {
  const state = channel.breaker_status || 'unknown'
  const until = channel.cooldown_until ? ` / ${formatDateTime(channel.cooldown_until)}` : ''
  return `${t(`admin.riskControl.breakerStates.${state}`)}${until}`
}

function channelTestResultText(result: TestDeepSeekChannelResponse): string {
  const passed = Number(result.safe_case?.passed) + Number(result.risk_case?.passed)
  return t(result.health_valid ? 'admin.riskControl.channelTestHealthy' : 'admin.riskControl.channelTestUnhealthy', {
    passed,
  })
}

function resultLabel(row: ContentModerationLog): string {
  if (row.review_outcome) return row.review_outcome
  if (row.action === 'review_unavailable' || row.error) return t('admin.riskControl.result.unavailable')
  if (row.action?.includes('shadow')) return t('admin.riskControl.result.shadow')
  if (row.action?.includes('block') || row.flagged) return t('admin.riskControl.result.blocked')
  return t('admin.riskControl.result.pass')
}

function resultClass(row: ContentModerationLog): string {
  if (row.action === 'review_unavailable' || row.error) return statusClasses.warning
  if (row.action?.includes('shadow')) return statusClasses.warning
  if (row.action?.includes('block') || row.flagged) return statusClasses.danger
  return statusClasses.healthy
}

function confidenceText(row: ContentModerationLog): string {
  const value = row.deepseek_confidence ?? row.highest_score
  return value === undefined ? '-' : percent(value)
}

function firstAttemptName(row: ContentModerationLog): string {
  const first = row.review_attempts?.[0]
  return first?.channel_name || first?.channel_id || first?.reviewer || row.provider || '-'
}

function attemptSummary(row: ContentModerationLog): string {
  const attempts = row.review_attempts ?? []
  if (attempts.length === 0) return '-'
  return t('admin.riskControl.attemptCount', { count: attempts.length })
}

function parseLineList(value: string): string[] {
  return value
    .split(/\r?\n/)
    .map((item) => item.trim())
    .filter((item, index, values) => item.length > 0 && values.indexOf(item) === index)
}

function parseEmailList(value: string): string[] {
  return parseLineList(value.toLowerCase())
}

function isValidChannelID(value: string): boolean {
  return /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(value)
}

function isAllowedBaseURL(value: string): boolean {
  try {
    const parsed = new URL(value)
    if (parsed.username || parsed.password || parsed.search || parsed.hash) return false
    if (parsed.protocol === 'https:') return true
    if (parsed.protocol !== 'http:') return false
    const host = parsed.hostname.toLowerCase().replace(/^\[|\]$/g, '')
    return host === 'localhost' || host === '::1' || /^127(?:\.\d{1,3}){3}$/.test(host)
  } catch {
    return false
  }
}

function clampInteger(value: number, min: number, max: number, fallback: number): number {
  if (!Number.isFinite(value)) return fallback
  return Math.max(min, Math.min(max, Math.trunc(value)))
}

function percent(value: number): string {
  if (!Number.isFinite(value)) return '-'
  return `${(value * 100).toFixed(0)}%`
}

function latencyText(value?: number | null): string {
  return value === undefined || value === null ? '-' : `${value} ms`
}

function formatDateTime(value: string): string {
  return formatDateTimeValue(value)
}

function dateOrDash(value?: string): string {
  return value ? formatDateTime(value) : '-'
}

function normalizeLocalDateTime(value: string): string | undefined {
  if (!value) return undefined
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString()
}

onMounted(() => {
  void loadInitial()
  refreshTimer = window.setInterval(() => {
    void loadStatus(true)
  }, 15000)
})

onUnmounted(() => {
  if (refreshTimer !== null) window.clearInterval(refreshTimer)
})
</script>
