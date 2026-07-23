<script>
  import {
    Activity,
    ArrowUp,
    BookOpen,
    Boxes,
    Braces,
    ChartNetwork,
    ChevronDown,
    ChevronRight,
    FileCode2,
    FileText,
    Folder,
    Maximize,
    Minimize,
    Minus,
    Moon,
    PanelLeftClose,
    PanelLeftOpen,
    Plus,
    RefreshCw,
    RotateCcw,
    Search,
    Settings,
    Sun,
    Tag,
    Warehouse,
    X,
  } from "@lucide/svelte";

  let { name, size = 18 } = $props();

  const icons = {
    activity: Activity,
    "arrow-up": ArrowUp,
    book: BookOpen,
    boxes: Boxes,
    braces: Braces,
    graph: ChartNetwork,
    "chevron-down": ChevronDown,
    "chevron-right": ChevronRight,
    "file-code": FileCode2,
    "file-text": FileText,
    folder: Folder,
    maximize: Maximize,
    minimize: Minimize,
    minus: Minus,
    moon: Moon,
    "panel-left-close": PanelLeftClose,
    "panel-left-open": PanelLeftOpen,
    plus: Plus,
    refresh: RefreshCw,
    reset: RotateCcw,
    search: Search,
    settings: Settings,
    sun: Sun,
    tag: Tag,
    warehouse: Warehouse,
    x: X,
  };

  let Component = $derived(icons[name]);
</script>

{#if Component}
  <Component {size} strokeWidth={1.8} aria-hidden="true" />
{/if}
