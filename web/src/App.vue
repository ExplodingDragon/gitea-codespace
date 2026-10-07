<script setup lang="ts">
import {onMounted, ref} from 'vue';
import {NButton, NConfigProvider, NDialogProvider, NMessageProvider, NInput, NAlert, NForm, NFormItem} from 'naive-ui';
import {Boxes, Globe, LogOut, Network, Container} from '@lucide/vue';
import {csrf, request} from './api';

const token = ref('');
const error = ref('');
const loading = ref(false);
const ready = ref(false);
const links = [
  {path: '/sites', label: 'Gitea sites', icon: Globe},
  {path: '/environments', label: 'Environment templates', icon: Boxes},
  {path: '/components', label: 'Components', icon: Network},
  {path: '/runtimes', label: 'Running environments', icon: Container},
];
onMounted(async () => {
  try {
    csrf.value = (await request<{csrf: string}>('session')).csrf;
  } catch (e) {
    if (!(e instanceof Error) || !e.message.includes('sign in')) error.value = String(e);
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
    error.value = (e as Error).message;
  } finally {
    loading.value = false;
  }
}
async function logout() {
  try {
    await request('session', 'DELETE');
    csrf.value = '';
  } catch (e) {
    error.value = (e as Error).message;
  }
}
</script>

<template>
  <NConfigProvider
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
          Loading administration...
        </div>
        <main
          v-else-if="!csrf"
          class="login"
        >
          <h1>Codespace Manager</h1>
          <NAlert
            v-if="error"
            type="error"
            class="notice"
          >
            {{ error }}
          </NAlert>
          <NForm @submit.prevent="login">
            <NFormItem label="Administration token">
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
              Sign in
            </NButton>
          </NForm>
        </main>
        <div
          v-else
          class="shell"
        >
          <header class="topbar">
            <strong>Codespace Manager</strong>
            <NButton
              quaternary
              @click="logout"
            >
              <template #icon><LogOut :size="16" /></template>
              Sign out
            </NButton>
          </header>
          <nav
            class="sidebar"
            aria-label="Administration"
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
