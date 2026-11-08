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
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import { Dialog } from '@/components/dialog'
import { JsonEditor } from '@/components/json-editor'
import { Button } from '@/components/ui/button'
import {
  Field,
  FieldDescription,
  FieldGroup,
  FieldLabel,
} from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'

import {
  operations,
  type CatalogEntry,
  type CatalogOperation,
  type Definition,
} from './api'

const jsonObject = z.string().refine((s) => {
  try {
    const v: unknown = JSON.parse(s)
    return v !== null && typeof v === 'object' && !Array.isArray(v)
  } catch {
    return false
  }
}, 'Enter a JSON object')
const schema = z.object({
  model: z.string().min(1).max(256),
  displayName: z.string(),
  owner: z.string(),
  operation: z.enum(operations),
  channelId: z.number().int().min(0),
  maxInputTokens: z.number().int().min(0),
  maxOutputTokens: z.number().int().min(0),
  protocol: z.string(),
  submitPath: z.string(),
  queryPathTemplate: z.string(),
  inputModalities: z.string(),
  outputModalities: z.string(),
  executionMode: z.string(),
  evidence: z.string(),
  inputSchema: jsonObject,
  outputSchema: jsonObject,
  limits: jsonObject,
  usageSchema: jsonObject,
  capabilities: jsonObject,
  usageBounds: jsonObject,
})
type Values = z.infer<typeof schema>
const jsonFields = [
  'inputSchema',
  'outputSchema',
  'limits',
  'usageSchema',
  'capabilities',
  'usageBounds',
] as const
const labels = {
  inputSchema: 'Input schema',
  outputSchema: 'Output schema',
  limits: 'Operation limits',
  usageSchema: 'Usage schema',
  capabilities: 'Declared capabilities',
  usageBounds: 'Proven usage bounds',
}
const textFields = {
  model: 'Model ID',
  displayName: 'Display name',
  owner: 'Configuration owner',
  protocol: 'Protocol',
  submitPath: 'Submit path',
  queryPathTemplate: 'Query path template',
  inputModalities: 'Input modalities',
  outputModalities: 'Output modalities',
  executionMode: 'Execution modes',
} as const
function commaList(value: string): string[] {
  return value
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
}

export function OperationEditor(props: {
  entry?: CatalogEntry
  operation?: CatalogOperation
  pending: boolean
  onClose: () => void
  onSave: (entry: CatalogEntry) => Promise<void>
}) {
  const { t } = useTranslation()
  const config = props.operation?.config
  const definition = config?.definition
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: {
      model: props.entry?.model ?? '',
      displayName: props.entry?.displayName ?? '',
      owner: props.entry?.owner ?? '',
      operation: props.operation?.operation ?? 'text_generation',
      channelId: config?.channelId ?? 0,
      maxInputTokens: config?.maxInputTokens ?? 0,
      maxOutputTokens: config?.maxOutputTokens ?? 0,
      protocol: definition?.protocol ?? '',
      submitPath: definition?.submitPath ?? '',
      queryPathTemplate: definition?.queryPathTemplate ?? '',
      inputModalities: definition?.inputModalities?.join(',') ?? '',
      outputModalities: definition?.outputModalities?.join(',') ?? '',
      executionMode: definition?.executionMode?.join(',') ?? '',
      evidence: props.operation?.evidence ?? '',
      inputSchema: JSON.stringify(definition?.inputSchema ?? {}),
      outputSchema: JSON.stringify(definition?.outputSchema ?? {}),
      limits: JSON.stringify(definition?.limits ?? {}),
      usageSchema: JSON.stringify(definition?.usageSchema ?? {}),
      capabilities: JSON.stringify(definition?.capabilities ?? {}),
      usageBounds: JSON.stringify(config?.usageBounds ?? {}),
    },
  })
  const submit = form.handleSubmit(async (v) => {
    const def: Definition = {
      operation: v.operation,
      operationRevision: 'configuration-v2',
      pricingRevision: 'resolved-at-runtime',
      inputSchema: JSON.parse(v.inputSchema),
      outputSchema: JSON.parse(v.outputSchema),
      limits: JSON.parse(v.limits),
      usageSchema: JSON.parse(v.usageSchema),
      capabilities: JSON.parse(v.capabilities),
      inputModalities: commaList(v.inputModalities),
      outputModalities: commaList(v.outputModalities),
      executionMode: commaList(v.executionMode),
      protocol: v.protocol,
      submitPath: v.submitPath,
      queryPathTemplate: v.queryPathTemplate,
    }
    await props.onSave({
      model: v.model,
      displayName: v.displayName || v.model,
      owner: v.owner,
      operations: [
        {
          operation: v.operation,
          state: 'draft',
          evidence: v.evidence,
          config: {
            model: v.model,
            channelId: v.channelId,
            definition: def,
            maxInputTokens: v.maxInputTokens,
            maxOutputTokens: v.maxOutputTokens,
            usageBounds: JSON.parse(v.usageBounds),
          },
        },
      ],
    })
  })
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !props.pending) props.onClose()
      }}
      title={t('Edit catalog operation')}
      description={t(
        'Save a draft first. Configuration validation does not prove real model admission.'
      )}
      contentClassName='sm:max-w-3xl'
      footer={
        <Button disabled={props.pending} onClick={() => void submit()}>
          {t('Save draft')}
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
          {(Object.keys(textFields) as Array<keyof typeof textFields>).map(
            (key) => (
              <Field key={key} data-invalid={!!form.formState.errors[key]}>
                <FieldLabel htmlFor={`catalog-${key}`}>
                  {t(textFields[key])}
                </FieldLabel>
                <Input
                  id={`catalog-${key}`}
                  {...form.register(key)}
                  readOnly={key === 'model' && !!props.entry}
                  aria-invalid={!!form.formState.errors[key]}
                />
                {form.formState.errors[key] && (
                  <FieldDescription>
                    {t(form.formState.errors[key]?.message ?? '')}
                  </FieldDescription>
                )}
              </Field>
            )
          )}
          <Field>
            <FieldLabel htmlFor='catalog-operation'>
              {t('Operation')}
            </FieldLabel>
            <NativeSelect
              id='catalog-operation'
              {...form.register('operation')}
              disabled={!!props.operation}
            >
              {operations.map((op) => (
                <NativeSelectOption key={op} value={op}>
                  {op}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </Field>
          {(['channelId', 'maxInputTokens', 'maxOutputTokens'] as const).map(
            (key) => (
              <Field key={key}>
                <FieldLabel htmlFor={`catalog-${key}`}>
                  {t(
                    {
                      channelId: 'Channel ID',
                      maxInputTokens: 'Maximum input tokens',
                      maxOutputTokens: 'Maximum output tokens',
                    }[key]
                  )}
                </FieldLabel>
                <Input
                  type='number'
                  min={0}
                  id={`catalog-${key}`}
                  {...form.register(key, { valueAsNumber: true })}
                  aria-invalid={!!form.formState.errors[key]}
                />
                {form.formState.errors[key] && (
                  <FieldDescription>
                    {t(form.formState.errors[key]?.message ?? '')}
                  </FieldDescription>
                )}
              </Field>
            )
          )}
          <Field>
            <FieldLabel htmlFor='catalog-evidence'>
              {t('Capability and billing evidence')}
            </FieldLabel>
            <Textarea id='catalog-evidence' {...form.register('evidence')} />
            <FieldDescription>
              {t(
                'Reference the exact route, limits and price evidence. Do not paste credentials.'
              )}
            </FieldDescription>
          </Field>
          {jsonFields.map((key) => (
            <Field key={key}>
              <FieldLabel>{t(labels[key])}</FieldLabel>
              <JsonEditor
                value={form.watch(key)}
                onChange={(value) =>
                  form.setValue(key, value, { shouldValidate: true })
                }
                valueType='any'
              />
              {form.formState.errors[key] && (
                <FieldDescription>
                  {t(form.formState.errors[key]?.message ?? '')}
                </FieldDescription>
              )}
            </Field>
          ))}
        </FieldGroup>
      </form>
    </Dialog>
  )
}
