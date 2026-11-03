<script setup lang="ts">
import {computed, onMounted, ref} from 'vue';
import {
  NButton,
  NConfigProvider,
  NDialogProvider,
  NMessageProvider,
  NInput,
  NAlert,
  NForm,
  NFormItem,
  NSelect,
  enUS,
  dateEnUS,
  zhCN,
  dateZhCN,
  jaJP,
  dateJaJP,
  koKR,
  dateKoKR,
} from 'naive-ui';
import {Boxes, Globe, LogOut, Network, Container, Languages} from '@lucide/vue';
import {useI18n} from 'vue-i18n';
import {APIError, csrf, errorMessage, request} from './api';
import {setLocale, supportedLocales} from './i18n';

const {t, locale} = useI18n();
const token = ref('');
const error = ref('');
const loading = ref(false);
const ready = ref(false);
const links = computed(() => [
  {path: '/sites', label: t('app.sites'), icon: Globe},
  {path: '/environments', label: t('app.environments'), icon: Boxes},
  {path: '/components', label: t('app.components'), icon: Network},
  {path: '/runtimes', label: t('app.runtimes'), icon: Container},
]);
const naiveLocales = {
  'en-US': [enUS, dateEnUS],
  'zh-CN': [zhCN, dateZhCN],
  'ja-JP': [jaJP, dateJaJP],
  'ko-KR': [koKR, dateKoKR],
} as const;
const naiveLocale = computed(() => naiveLocales[locale.value as keyof typeof naiveLocales] ?? naiveLocales['en-US']);
onMounted(async () => {
  try {
    csrf.value = (await request<{csrf: string}>('session')).csrf;
  } catch (e) {
    if (!(e instanceof APIError) || (e.code !== 'authentication_required' && e.code !== 'session_expired')) {
      error.value = errorMessage(e, t);
    }
  } finally {
    ready.value = true;
  }
});
async function login() {
  loading.value = true;
  error.value = '';
  try {
    csrf.value = (await request<{csrf: string}>('login', 'POST', {token: token.value})).csrf;
    token.value = '';
  } catch (e) {
    error.value = errorMessage(e, t);
  } finally {
    loading.value = false;
  }
}
async function logout() {
  try {
    await request('session', 'DELETE');
    csrf.value = '';
  } catch (e) {
    error.value = errorMessage(e, t);
  }
}
</script>

<template>
  <NConfigProvider
    :locale="naiveLocale[0]"
    :date-locale="naiveLocale[1]"
    :theme-overrides="{
      common: {
        primaryColor: '#238636',
        primaryColorHover: '#2da144',
        primaryColorPressed: '#196c2e',
        borderRadius: '4px',
      },
    }"
  >
    <NMessageProvider>
      <NDialogProvider>
        <div
          v-if="!ready"
          class="loading"
        >
          {{ t('app.loading') }}
        </div>
        <main
          v-else-if="!csrf"
          class="login"
        >
          <div class="login-language">
            <Languages :size="16" />
            <NSelect
              class="language-select"
              :value="locale"
              :options="supportedLocales"
              :aria-label="t('app.language')"
              @update:value="setLocale"
            />
          </div>
          <h1>{{ t('app.title') }}</h1>
          <NAlert
            v-if="error"
            type="error"
            class="notice"
          >
            {{ error }}
          </NAlert>
          <NForm @submit.prevent="login">
            <NFormItem :label="t('app.token')">
              <NInput
                v-model:value="token"
                type="password"
                autocomplete="current-password"
                show-password-on="click"
                autofocus
              />
            </NFormItem>
            <NButton
              type="primary"
              attr-type="submit"
              :loading="loading"
              :disabled="!token"
            >
              {{ t('app.signIn') }}
            </NButton>
          </NForm>
        </main>
        <div
          v-else
          class="shell"
        >
          <header class="topbar">
            <strong>{{ t('app.title') }}</strong>
            <div class="topbar-actions">
              <Languages :size="16" />
              <NSelect
                class="language-select"
                :value="locale"
                :options="supportedLocales"
                :aria-label="t('app.language')"
                @update:value="setLocale"
              />
              <NButton
                quaternary
                :aria-label="t('app.signOut')"
                :title="t('app.signOut')"
                @click="logout"
              >
                <template #icon><LogOut :size="16" /></template>
                <span class="sign-out-label">{{ t('app.signOut') }}</span>
              </NButton>
            </div>
          </header>
          <nav
            class="sidebar"
            :aria-label="t('app.navigation')"
          >
            <RouterLink
              v-for="link in links"
              :key="link.path"
              :to="link.path"
            >
              <component
                :is="link.icon"
                :size="18"
              />
              <span>{{ link.label }}</span>
            </RouterLink>
          </nav>
          <main class="content"><RouterView /></main>
        </div>
      </NDialogProvider>
    </NMessageProvider>
  </NConfigProvider>
</template>
