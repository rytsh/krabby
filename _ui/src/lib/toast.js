import { writable } from "svelte/store";

export const toasts = writable([]);

let nextId = 0;

export function dismissToast(id) {
  toasts.update((items) => items.filter((item) => item.id !== id));
}

export function errorToast(error) {
  const message = error instanceof Error ? error.message : String(error || "Something went wrong");

  toasts.update((items) => {
    if (items.some((item) => item.type === "error" && item.message === message)) return items;

    const id = ++nextId;
    setTimeout(() => dismissToast(id), 6000);

    return [...items.slice(-3), { id, type: "error", message }];
  });
}
