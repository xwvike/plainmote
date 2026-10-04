// Unlocking from any page that needs the account key: read the keyring,
// open it with the master password, keep the key in this browser.
import { openWithPassword } from "./keys.js";
import { put, get } from "./vault.js";

export const currentUser = () => document.querySelector('meta[name="plainmote-user"]')?.content || "";

export const accountKey = (user = currentUser()) => get(user);

export class WrongPassword extends Error {}
export class NoStorage extends Error {}

export async function unlock(password, user = currentUser()) {
  const response = await fetch("/account/keyring", { headers: { Accept: "application/json" }, credentials: "same-origin" });
  if (!response.ok) throw new Error("keyring unavailable");
  const ring = await response.json();
  ring.iterations = Number(ring.iterations);
  let key;
  try {
    key = await openWithPassword(user, ring, password);
  } catch {
    throw new WrongPassword();
  }
  if (!(await put(user, key, ring.lock_minutes))) throw new NoStorage();
  return key;
}
