/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { render } from '@testing-library/react'
import { expect, it } from 'vitest'

import { WebPreview, WebPreviewBody } from '../web-preview'

it('keeps the preview opaque even if an untyped caller passes a sandbox override', () => {
  const props = {
    src: 'https://preview.example',
    sandbox: 'allow-same-origin allow-scripts',
  }
  const { container } = render(
    <WebPreview>
      <WebPreviewBody {...props} />
    </WebPreview>
  )
  const frame = container.querySelector('iframe')
  expect(frame).toHaveAttribute(
    'sandbox',
    'allow-scripts allow-forms allow-popups allow-presentation'
  )
  expect(frame).toHaveAttribute('src', 'https://preview.example')
})
