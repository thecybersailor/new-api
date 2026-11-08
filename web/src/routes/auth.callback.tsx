import { createFileRoute, redirect } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { exchangeCapturedDanQiuOSCallback } from '@/features/auth/lib/danqiuos'

function CallbackError() {
  const { t } = useTranslation()
  return <ErrorState title={t('OAuth failed')} />
}

export const Route = createFileRoute('/auth/callback')({
  beforeLoad: async () => {
    const href = await exchangeCapturedDanQiuOSCallback()
    throw redirect({ href, replace: true })
  },
  errorComponent: CallbackError,
})
