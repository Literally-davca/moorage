<script lang="ts">
  import { untrack } from 'svelte';
  import { EditorState } from '@codemirror/state';
  import { EditorView, keymap, lineNumbers, highlightActiveLine, highlightActiveLineGutter, drawSelection } from '@codemirror/view';
  import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands';
  import { HighlightStyle, syntaxHighlighting, indentOnInput, bracketMatching } from '@codemirror/language';
  import { yaml } from '@codemirror/lang-yaml';
  import { tags as t } from '@lezer/highlight';

  let {
    value = $bindable(''),
    language = 'yaml',
    readonly = false,
    label,
    minHeight = 320,
  }: { value: string; language?: 'yaml' | 'plain'; readonly?: boolean; label: string; minHeight?: number } = $props();

  let host: HTMLDivElement;
  let view: EditorView | undefined;

  // Colours come from the Moorage tokens so the editor follows the theme.
  const theme = EditorView.theme({
    '&': { backgroundColor: 'var(--well)', color: 'var(--foam)', borderRadius: 'var(--r-card)', fontSize: '13px' },
    '&.cm-focused': { outline: 'none', boxShadow: '0 0 0 2px var(--buoy), 0 0 0 5px var(--buoy-wash)' },
    '.cm-scroller': { fontFamily: 'var(--mono)', lineHeight: '1.65', padding: '8px 0' },
    '.cm-gutters': { backgroundColor: 'transparent', color: 'var(--murk)', border: 'none', paddingLeft: '6px' },
    '.cm-activeLine': { backgroundColor: 'var(--rope)' },
    '.cm-activeLineGutter': { backgroundColor: 'transparent', color: 'var(--mist)' },
    '.cm-cursor': { borderLeftColor: 'var(--buoy)', borderLeftWidth: '2px' },
    '.cm-selectionBackground, &.cm-focused .cm-selectionBackground': { backgroundColor: 'var(--buoy-wash) !important' },
    '.cm-content': { caretColor: 'var(--buoy)' },
  });

  const highlight = HighlightStyle.define([
    { tag: [t.propertyName, t.definition(t.propertyName)], color: 'var(--foam)', fontWeight: '500' },
    { tag: [t.string, t.special(t.string)], color: 'var(--afloat-text)' },
    { tag: [t.number, t.bool, t.null], color: 'var(--buoy-text)' },
    { tag: [t.comment, t.lineComment], color: 'var(--murk)', fontStyle: 'italic' },
    { tag: [t.punctuation, t.separator, t.squareBracket, t.brace], color: 'var(--haze)' },
    { tag: [t.meta, t.labelName, t.typeName], color: 'var(--signal-text)' },
  ]);

  // Created once; later value changes are applied by the effect below, never by re-creating the editor.
  $effect(() => {
    const initial = untrack(() => value);
    const ro = untrack(() => readonly);
    view = new EditorView({
      parent: host,
      state: EditorState.create({
        doc: initial,
        extensions: [
          lineNumbers(),
          highlightActiveLineGutter(),
          highlightActiveLine(),
          drawSelection(),
          history(),
          indentOnInput(),
          bracketMatching(),
          keymap.of([indentWithTab, ...defaultKeymap, ...historyKeymap]),
          language === 'yaml' ? yaml() : [],
          syntaxHighlighting(highlight),
          theme,
          EditorState.tabSize.of(2),
          EditorState.readOnly.of(ro),
          EditorView.contentAttributes.of({ 'aria-label': label }),
          EditorView.updateListener.of((u) => {
            if (u.docChanged) value = u.state.doc.toString();
          }),
        ],
      }),
    });
    return () => view?.destroy();
  });

  // external value changes (reload, convert) replace the document
  $effect(() => {
    const v = value;
    if (view && v !== view.state.doc.toString()) {
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: v } });
    }
  });
</script>

<div class="editor" bind:this={host} style="--min:{minHeight}px"></div>

<style>
  .editor {
    min-width: 0;
  }
  .editor :global(.cm-editor) {
    max-width: 100%;
    min-height: var(--min);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .editor :global(.cm-scroller) {
    min-height: var(--min);
  }
</style>
