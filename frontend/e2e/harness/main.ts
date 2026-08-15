import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
import i18n, { initI18n } from '../../src/i18n'

async function bootstrap() {
  localStorage.setItem('sub2api_locale', 'en')
  await initI18n()

  createApp(App)
    .use(createPinia())
    .use(i18n)
    .mount('#app')
}

void bootstrap()
