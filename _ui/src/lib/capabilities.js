import { writable } from "svelte/store";

// Keep mutation controls hidden until the server's capabilities are known.
export const readOnly = writable(true);
