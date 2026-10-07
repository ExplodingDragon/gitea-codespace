import {createApp} from 'vue';
import {createRouter, createWebHistory} from 'vue-router';
import App from './App.vue';
import './style.css';

const router = createRouter({
  history: createWebHistory(),
  routes: [
    {path: '/', redirect: '/sites'},
    {path: '/components', component: () => import('./pages/ComponentsPage.vue')},
    {path: '/sites/:id?', component: () => import('./pages/SitesPage.vue')},
    {path: '/environments/:id?', component: () => import('./pages/EnvironmentsPage.vue')},
    {path: '/runtimes', component: () => import('./pages/RuntimesPage.vue')},
    {path: '/:pathMatch(.*)*', redirect: '/sites'},
  ],
});
createApp(App).use(router).mount('#app');
