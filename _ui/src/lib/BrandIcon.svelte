<script>
  import { siGithub } from "simple-icons/icons";

  let { name, size = 18 } = $props();

  const icons = {
    github: siGithub,
  };

  let icon = $derived(icons[name]);
</script>

{#if icon}
  <svg width={size} height={size} viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
    <path d={icon.path} />
  </svg>
{/if}
