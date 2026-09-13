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
import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import type { ControllerRenderProps, UseFormReturn } from 'react-hook-form'

import {
  SideDrawerSection,
  SideDrawerSectionHeader,
} from '@/components/drawer-layout'
import {
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { MultiSelect } from '@/components/multi-select'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'

import {
  parseModelsString,
  type ChannelBatchEditFormValues,
} from '../../lib'

type SelectOption = { value: string; label: string }

type ChannelBatchEditFieldsProps = {
  form: UseFormReturn<ChannelBatchEditFormValues>
  groupOptions: SelectOption[]
  modelOptions: SelectOption[]
}

/**
 * Comma separated multi select: the form value is a plain string, so the option
 * list is memoized per value instead of rebuilt on every keystroke elsewhere.
 */
function CommaListMultiSelect(props: {
  field: ControllerRenderProps<ChannelBatchEditFormValues, 'group' | 'models'>
  options: SelectOption[]
  placeholder: string
  allowCreate?: boolean
}) {
  const selected = useMemo(
    () => parseModelsString(props.field.value ?? ''),
    [props.field.value]
  )

  return (
    <MultiSelect
      options={props.options}
      selected={selected}
      onChange={(values) => props.field.onChange(values.join(','))}
      placeholder={props.placeholder}
      allowCreate={props.allowCreate}
      createLabel={
        props.allowCreate ? 'Add custom model "{{value}}"' : undefined
      }
      maxVisibleChips={props.allowCreate ? 8 : undefined}
    />
  )
}

export function ChannelBatchEditFields(props: ChannelBatchEditFieldsProps) {
  const { t } = useTranslation()

  const autoBanOptions: SelectOption[] = [
    { value: 'unchanged', label: t('Keep unchanged') },
    { value: 'enabled', label: t('Enabled') },
    { value: 'disabled', label: t('Disabled') },
  ]

  return (
    <>
      <SideDrawerSection>
        <SideDrawerSectionHeader title={t('Basic Information')} />

        <FormField
          control={props.form.control}
          name='group'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Group')}</FormLabel>
              <FormControl>
                <CommaListMultiSelect
                  field={field}
                  options={props.groupOptions}
                  placeholder={t('Select groups (leave empty to keep current)')}
                />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />

        <FormField
          control={props.form.control}
          name='tag'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Tag')}</FormLabel>
              <FormControl>
                <Input
                  {...field}
                  value={field.value ?? ''}
                  placeholder={t('Enter tag name (optional)')}
                />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />

        <FormField
          control={props.form.control}
          name='remark'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Remark')}</FormLabel>
              <FormControl>
                <Input
                  {...field}
                  value={field.value ?? ''}
                  placeholder={t('Enter remark (optional)')}
                />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />
      </SideDrawerSection>

      <SideDrawerSection>
        <SideDrawerSectionHeader title={t('Models')} />

        <FormField
          control={props.form.control}
          name='models'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Models')}</FormLabel>
              <FormControl>
                <CommaListMultiSelect
                  field={field}
                  options={props.modelOptions}
                  placeholder={t('Select models or add custom ones')}
                  allowCreate
                />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />

        <FormField
          control={props.form.control}
          name='model_mapping'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Model Mapping')}</FormLabel>
              <FormControl>
                <Textarea
                  {...field}
                  value={field.value ?? ''}
                  placeholder='{"gpt-4o":"gpt-4o-mini"}'
                  rows={3}
                />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />
      </SideDrawerSection>

      <SideDrawerSection>
        <SideDrawerSectionHeader title={t('Advanced Settings')} />

        <div className='grid gap-4 sm:grid-cols-2'>
          <FormField
            control={props.form.control}
            name='weight'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Weight')}</FormLabel>
                <FormControl>
                  <Input
                    {...field}
                    value={field.value ?? ''}
                    inputMode='numeric'
                    placeholder='0'
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />

          <FormField
            control={props.form.control}
            name='priority'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Priority')}</FormLabel>
                <FormControl>
                  <Input
                    {...field}
                    value={field.value ?? ''}
                    inputMode='numeric'
                    placeholder='0'
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
        </div>

        <FormField
          control={props.form.control}
          name='test_model'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Test Model')}</FormLabel>
              <FormControl>
                <Input
                  {...field}
                  value={field.value ?? ''}
                  placeholder={t('Enter the model used for channel testing')}
                />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />

        <FormField
          control={props.form.control}
          name='auto_ban'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Auto Ban')}</FormLabel>
              <Select
                value={field.value ?? 'unchanged'}
                onValueChange={field.onChange}
                items={autoBanOptions}
              >
                <FormControl>
                  <SelectTrigger className='w-full'>
                    <SelectValue />
                  </SelectTrigger>
                </FormControl>
                <SelectContent alignItemWithTrigger={false}>
                  <SelectGroup>
                    {autoBanOptions.map((option) => (
                      <SelectItem key={option.value} value={option.value}>
                        {option.label}
                      </SelectItem>
                    ))}
                  </SelectGroup>
                </SelectContent>
              </Select>
              <FormMessage />
            </FormItem>
          )}
        />
      </SideDrawerSection>
    </>
  )
}
