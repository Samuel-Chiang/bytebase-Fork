<template>
  <div
    :id="domIDForItem(item)"
    class="flex items-start justify-between hover:bg-gray-100 px-1 gap-x-1"
    :class="[isCurrentItem && 'bg-indigo-600/10']"
    @click="$emit('click', item, $event)"
  >
    <SheetConnectionIcon :tab="item.target" class="shrink-0 w-4 h-6" />

    <div class="flex-1 text-sm leading-6 cursor-pointer truncate">
      <!-- eslint-disable-next-line vue/no-v-html -->
      <span v-html="titleHTML(item, keyword)" />
    </div>

    <div class="shrink-0 w-6 h-6 flex items-center justify-center" @click.stop>
      <NTooltip>
        <template #trigger>
          <carbon:dot-mark class="text-accent w-4 h-4" />
        </template>
        <template #default>
          <span>{{ $t("sql-editor.tab.unsaved") }}</span>
        </template>
      </NTooltip>
    </div>
  </div>
</template>

<script setup lang="ts">
import { NTooltip } from "naive-ui";
import { SheetConnectionIcon } from "@/views/sql-editor/EditorCommon";
import { MergedItem, TabItem, domIDForItem, titleHTML } from "./common";

defineProps<{
  item: TabItem;
  isCurrentItem: boolean;
  keyword: string;
}>();

defineEmits<{
  (event: "click", item: MergedItem, e: MouseEvent): void;
}>();
</script>
