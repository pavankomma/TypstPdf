import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'

// Load order matters: fonts → tokens → app layer.
import './styles/fonts.css'
import './styles/doclerk.css'
import './styles/app.css'

createApp(App).use(createPinia()).mount('#app')
