import { defineStore } from 'pinia'
import { ref } from 'vue'
import { getPublicSystemConfig } from '@/api/system'

export const defaultLogoUrl = '/favicon.svg'
const defaultSiteTitle = 'yagp'
const previousDefaultLogoUrl = 'https://gost.run/images/gost.png'

function logoOverride(url) {
    return url === previousDefaultLogoUrl ? '' : (url || '')
}

export const useSystemStore = defineStore('system', () => {
    // State
    const siteTitle = ref(localStorage.getItem('siteTitle') || defaultSiteTitle)
    const logoUrl = ref(logoOverride(localStorage.getItem('logoUrl')))
    const copyright = ref(localStorage.getItem('copyright') || '')
    const version = ref('')

    // Actions
    async function fetchSystemConfig() {
        try {
            const res = await getPublicSystemConfig()
            if (res.data) {
                const config = res.data
                version.value = config.version || ''

                // Update state
                if (config.siteTitle !== undefined) {
                    siteTitle.value = config.siteTitle || defaultSiteTitle
                    document.title = siteTitle.value
                    if (config.siteTitle) {
                        localStorage.setItem('siteTitle', config.siteTitle)
                    } else {
                        localStorage.removeItem('siteTitle')
                    }
                }

                logoUrl.value = logoOverride(config.logoUrl)
                if (logoUrl.value) {
                    localStorage.setItem('logoUrl', logoUrl.value)
                } else {
                    localStorage.removeItem('logoUrl')
                }

                if (config.copyright !== undefined) {
                    copyright.value = config.copyright || ''
                    if (copyright.value) {
                        localStorage.setItem('copyright', copyright.value)
                    } else {
                        localStorage.removeItem('copyright')
                    }
                }
            }
        } catch (error) {
            console.error('获取系统配置失败:', error)
        }
    }

    // Initialize document title from storage immediately
    document.title = siteTitle.value

    return {
        siteTitle,
        logoUrl,
        copyright,
        version,
        fetchSystemConfig
    }
})
