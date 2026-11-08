import { api } from '@/lib/api'
import {
  AuthOperationError,
  authRequestOptions,
  authResult,
} from '@/lib/secure-verification'

// The fragment is removed before parsing or any request. Never persist the
// central access/refresh token or trust the transfer's user/role fields.
function readDanQiuOSCallback(
  location: Pick<Location, 'hash' | 'search' | 'pathname'>,
  history: Pick<History, 'replaceState'>
): { state: string; access_token: string } {
  const fragment = location.hash
  const query = location.search
  history.replaceState(null, '', location.pathname)
  const state = new URLSearchParams(query).get('state')
  const encoded = new URLSearchParams(fragment.replace(/^#/, '')).get(
    'account_session'
  )
  if (!state || !encoded || encoded.length > 65536) {
    throw new AuthOperationError('OAuth failed')
  }
  let transfer: unknown
  try {
    transfer = JSON.parse(encoded)
  } catch {
    throw new AuthOperationError('OAuth failed')
  }
  if (
    !transfer ||
    typeof transfer !== 'object' ||
    !('session' in transfer) ||
    !transfer.session ||
    typeof transfer.session !== 'object' ||
    !('access_token' in transfer.session) ||
    typeof transfer.session.access_token !== 'string' ||
    !transfer.session.access_token ||
    transfer.session.access_token.length > 32768
  ) {
    throw new AuthOperationError('OAuth failed')
  }
  return { state, access_token: transfer.session.access_token }
}

async function exchangeTransfer(transfer: {
  state: string
  access_token: string
}): Promise<string> {
  const state = transfer.state
  const assertion = await authResult<{ code: string }>(
    api.post(
      '/api/danqiuos/assertion',
      {
        state,
        access_token: transfer.access_token,
      },
      {
        ...authRequestOptions,
        // This is an anonymous credential exchange. The authenticated proof
        // interceptor would require an existing local session before sign-in.
        skipAuthRefresh: true,
      }
    )
  )
  if (!assertion.code) {
    throw new AuthOperationError('OAuth failed')
  }
  return `/oauth/danqiuos?${new URLSearchParams({ state, code: assertion.code })}`
}

let captured: ReturnType<typeof readDanQiuOSCallback> | undefined
let captureError: unknown
let exchange: Promise<string> | undefined

// Run before the router subscribes to history: replacing the URL in a route
// loader would retrigger navigation and cancel the one-time exchange.
export function captureDanQiuOSCallback(
  location: Pick<Location, 'hash' | 'search' | 'pathname'>,
  history: Pick<History, 'replaceState'>
): void {
  if (location.pathname !== '/auth/callback') {
    return
  }
  try {
    captured = readDanQiuOSCallback(location, history)
  } catch (error) {
    captureError = error
  }
}

export function exchangeCapturedDanQiuOSCallback(): Promise<string> {
  if (!exchange) {
    if (captureError || !captured) {
      return Promise.reject(new AuthOperationError('OAuth failed'))
    }
    const transfer = captured
    captured = undefined
    exchange = exchangeTransfer(transfer)
  }
  return exchange
}

export async function exchangeDanQiuOSCallback(
  location: Pick<Location, 'hash' | 'search' | 'pathname'>,
  history: Pick<History, 'replaceState'>
): Promise<string> {
  return exchangeTransfer(readDanQiuOSCallback(location, history))
}
