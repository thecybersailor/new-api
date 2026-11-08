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
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import { CopyButton } from '@/components/copy-button'
import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Field, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { formatNumber } from '@/lib/format'
import { handleServerError } from '@/lib/handle-server-error'

import {
  directoryKey,
  getBindings,
  operations,
  saveBinding,
  type Binding,
} from './api'

const bindingKey = ['catalog-bindings']
const schema = z.object({
  tokenId: z.number().int().positive(),
  userId: z.number().int().positive(),
  principalId: z.string().min(1).max(128),
  fixedGroup: z.string().min(1).max(128),
  enabled: z.boolean(),
  modelPolicy: z.enum(['all_in_group', 'allowlist']),
  models: z.string(),
  ops: z.array(z.string()),
  reason: z.string().min(1),
})
type Values = z.infer<typeof schema>
export function CatalogBindings() {
  const { t } = useTranslation()
  const query = useQuery({ queryKey: bindingKey, queryFn: getBindings })
  const [editing, setEditing] = useState<Binding | null | undefined>()
  if (query.isPending) return <LoadingState />
  if (query.isError) return <ErrorState onRetry={() => void query.refetch()} />
  return (
    <section className='grid gap-3'>
      <h3 className='font-semibold'>{t('Integration authorization')}</h3>
      <p>
        {t(
          'Binding identity is immutable. Model and operation rules are separate from supplier availability.'
        )}
      </p>
      <Button variant='outline' onClick={() => setEditing(null)}>
        {t('Add integration binding')}
      </Button>
      <StaticDataTable
        data={query.data ?? []}
        getRowKey={(row) => row.tokenId}
        columns={[
          {
            id: 'identity',
            header: t('Integration principal'),
            cell: (row) => (
              <div className='flex items-center gap-2'>
                <span>{row.principalId}</span>
                <CopyButton
                  value={JSON.stringify(
                    {
                      integrationPrincipalId: row.principalId,
                      scopeId: row.scopeId,
                      bindingVersion: row.bindingVersion,
                    },
                    null,
                    2
                  )}
                />
              </div>
            ),
          },
          {
            id: 'group',
            header: t('Fixed group'),
            cell: (row) => row.fixedGroup,
          },
          {
            id: 'token',
            header: t('Token ID'),
            cell: (row) => formatNumber(row.tokenId),
          },
          {
            id: 'policy',
            header: t('Model policy'),
            cell: (row) => row.modelPolicy || 'all_in_group',
          },
          {
            id: 'status',
            header: t('Status'),
            cell: (row) => (row.enabled ? t('Enabled') : t('Disabled')),
          },
          {
            id: 'edit',
            header: t('Actions'),
            cell: (row) => (
              <Button variant='outline' onClick={() => setEditing(row)}>
                {t('Edit authorization')}
              </Button>
            ),
          },
        ]}
      />
      {editing !== undefined && (
        <BindingEditor
          binding={editing}
          onClose={() => setEditing(undefined)}
        />
      )}
    </section>
  )
}
function BindingEditor(props: {
  binding: Binding | null
  onClose: () => void
}) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const binding = props.binding
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: {
      tokenId: binding?.tokenId ?? 0,
      userId: binding?.userId ?? 0,
      principalId: binding?.principalId ?? '',
      fixedGroup: binding?.fixedGroup ?? '',
      enabled: binding?.enabled ?? true,
      modelPolicy:
        binding?.modelPolicy === 'allowlist' ? 'allowlist' : 'all_in_group',
      models: binding?.allowedModels
        ? (JSON.parse(binding.allowedModels) as string[]).join('\n')
        : '',
      ops: binding?.allowedOperations
        ? (JSON.parse(binding.allowedOperations) as string[])
        : [...operations],
      reason: '',
    },
  })
  const mutation = useMutation({
    mutationFn: async (v: Values) =>
      saveBinding(
        {
          tokenId: v.tokenId,
          userId: v.userId,
          principalId: v.principalId,
          fixedGroup: v.fixedGroup,
          enabled: v.enabled,
          revision: binding?.revision ?? 0,
          modelPolicy: v.modelPolicy,
          allowedModels: JSON.stringify(
            v.models
              .split('\n')
              .map((s) => s.trim())
              .filter(Boolean)
          ),
          allowedOperations: JSON.stringify(v.ops),
        },
        v.reason
      ),
    onError: (e) => handleServerError(e),
    onSuccess: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: bindingKey }),
        client.invalidateQueries({ queryKey: directoryKey }),
      ])
      props.onClose()
    },
  })
  const submit = form.handleSubmit((v) => mutation.mutate(v))
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !mutation.isPending) props.onClose()
      }}
      title={t('Edit authorization')}
      description={t(
        'Reduced permissions stop new calls. Disabling a binding also denies access through that credential.'
      )}
      footer={
        <Button disabled={mutation.isPending} onClick={() => void submit()}>
          {t('Confirm authorization change')}
        </Button>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void submit()
        }}
      >
        <FieldGroup>
          {(['tokenId', 'userId'] as const).map((key) => (
            <Field key={key}>
              <FieldLabel htmlFor={`binding-${key}`}>
                {t(key === 'tokenId' ? 'Token ID' : 'Account ID')}
              </FieldLabel>
              <Input
                id={`binding-${key}`}
                type='number'
                min={1}
                {...form.register(key, { valueAsNumber: true })}
                readOnly={!!binding}
              />
            </Field>
          ))}
          {(['principalId', 'fixedGroup'] as const).map((key) => (
            <Field key={key}>
              <FieldLabel htmlFor={`binding-${key}`}>
                {t(
                  key === 'principalId'
                    ? 'Integration principal'
                    : 'Fixed group'
                )}
              </FieldLabel>
              <Input
                id={`binding-${key}`}
                {...form.register(key)}
                readOnly={!!binding}
              />
            </Field>
          ))}
          <Field>
            <FieldLabel htmlFor='binding-policy'>
              {t('Model policy')}
            </FieldLabel>
            <NativeSelect id='binding-policy' {...form.register('modelPolicy')}>
              <NativeSelectOption value='all_in_group'>
                {t('All models in the authorized group')}
              </NativeSelectOption>
              <NativeSelectOption value='allowlist'>
                {t('Explicit model list')}
              </NativeSelectOption>
            </NativeSelect>
          </Field>
          {form.watch('modelPolicy') === 'allowlist' && (
            <Field>
              <FieldLabel htmlFor='binding-models'>
                {t('Allowed models, one ID per line')}
              </FieldLabel>
              <Textarea id='binding-models' {...form.register('models')} />
            </Field>
          )}
          <Field>
            <FieldLabel>{t('Allowed operations')}</FieldLabel>
            {operations.map((op) => (
              <label key={op} className='flex items-center gap-2'>
                <Checkbox
                  checked={form.watch('ops').includes(op)}
                  onCheckedChange={(checked) =>
                    form.setValue(
                      'ops',
                      checked
                        ? [...form.getValues('ops'), op]
                        : form.getValues('ops').filter((v) => v !== op)
                    )
                  }
                />
                {op}
              </label>
            ))}
          </Field>
          <Field orientation='horizontal'>
            <Checkbox
              id='binding-enabled'
              checked={form.watch('enabled')}
              onCheckedChange={(checked) => form.setValue('enabled', !!checked)}
            />
            <FieldLabel htmlFor='binding-enabled'>{t('Enabled')}</FieldLabel>
          </Field>
          <Field>
            <FieldLabel htmlFor='binding-reason'>
              {t('Change reason')}
            </FieldLabel>
            <Textarea id='binding-reason' {...form.register('reason')} />
          </Field>
          {Object.keys(form.formState.errors).length > 0 && (
            <p role='alert'>
              {t('Check the required identity fields and change reason.')}
            </p>
          )}
        </FieldGroup>
      </form>
    </Dialog>
  )
}
