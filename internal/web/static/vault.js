// Where an unlocked account key waits between pages: this site's IndexedDB,
// as the non-extractable CryptoKey itself, with when it was last used and how
// long it may sit idle. Nothing is stored before the first unlock; when the
// last key goes, so does the database.
//
// Every call tolerates IndexedDB being unavailable - a private window, a
// blocked site - by acting as if nothing were unlocked.

const NAME = "plainmote";
const STORE = "keys";

function request(req) {
  return new Promise((resolve, reject) => {
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

// present reports whether the database exists, without creating it. A
// browser that cannot say is treated as having it, which only costs an
// empty database being created and removed again.
export async function present() {
  if (!globalThis.indexedDB) return false;
  if (!indexedDB.databases) return true;
  try {
    return (await indexedDB.databases()).some((db) => db.name === NAME);
  } catch {
    return false;
  }
}

async function open() {
  const req = indexedDB.open(NAME, 1);
  req.onupgradeneeded = () => req.result.createObjectStore(STORE);
  return request(req);
}

async function withStore(mode, fn) {
  const db = await open();
  try {
    const tx = db.transaction(STORE, mode);
    const result = await fn(tx.objectStore(STORE));
    await new Promise((resolve, reject) => {
      tx.oncomplete = resolve;
      tx.onerror = () => reject(tx.error);
      tx.onabort = () => reject(tx.error);
    });
    return result;
  } finally {
    db.close();
  }
}

const expired = (record, now) => !record || now - record.lastUsed > record.lockMinutes * 60000;

export async function put(user, key, lockMinutes) {
  try {
    await withStore("readwrite", (store) => request(store.put({ key, lastUsed: Date.now(), lockMinutes }, user)));
    return true;
  } catch {
    return false;
  }
}

// get returns the user's key if it is there and has not sat idle too long,
// and counts this as a use. An idle one is removed on the way.
export async function get(user) {
  if (!(await present())) return null;
  try {
    const now = Date.now();
    const record = await withStore("readwrite", async (store) => {
      const found = await request(store.get(user));
      if (expired(found, now)) {
        if (found) await request(store.delete(user));
        return null;
      }
      found.lastUsed = now;
      await request(store.put(found, user));
      return found;
    });
    return record ? record.key : null;
  } catch {
    return null;
  }
}

// touch moves the idle clock on without reading the key: the person is here.
export async function touch(user) {
  if (!(await present())) return false;
  try {
    return await withStore("readwrite", async (store) => {
      const found = await request(store.get(user));
      if (expired(found, Date.now())) return false;
      found.lastUsed = Date.now();
      await request(store.put(found, user));
      return true;
    });
  } catch {
    return false;
  }
}

export async function setLockMinutes(user, minutes) {
  if (!(await present())) return;
  try {
    await withStore("readwrite", async (store) => {
      const found = await request(store.get(user));
      if (found) await request(store.put({ ...found, lockMinutes: minutes }, user));
    });
  } catch {
    // Nothing unlocked here; the next unlock takes the new time.
  }
}

// sweep keeps only the signed-in user's key, and only while it is fresh.
// Signed out (user empty), everything goes - and the database with it.
export async function sweep(user) {
  if (!(await present())) return false;
  let kept = false;
  try {
    const now = Date.now();
    kept = await withStore("readwrite", async (store) => {
      const users = await request(store.getAllKeys());
      let keep = false;
      for (const name of users) {
        const record = await request(store.get(name));
        if (name === user && !expired(record, now)) keep = true;
        else await request(store.delete(name));
      }
      return keep;
    });
  } catch {
    kept = false;
  }
  if (!kept) await drop();
  return kept;
}

// alive reports whether the user's key is still there and fresh, without
// counting as a use; a stale one is removed.
export async function alive(user) {
  if (!(await present())) return false;
  try {
    return await withStore("readwrite", async (store) => {
      const found = await request(store.get(user));
      if (!expired(found, Date.now())) return true;
      if (found) await request(store.delete(user));
      return false;
    });
  } catch {
    return false;
  }
}

// lock removes the user's key now - and with no other key left, the
// database.
export async function lock(user) {
  if (!(await present())) return;
  try {
    await withStore("readwrite", (store) => request(store.delete(user)));
  } catch {
    // Already gone.
  }
  await sweep(user);
}

// drop removes the database. Another tab holding it open blocks the delete
// until it closes; the keys in it are already gone, so that is not waited for.
async function drop() {
  try {
    await new Promise((resolve) => {
      const req = indexedDB.deleteDatabase(NAME);
      req.onsuccess = req.onerror = req.onblocked = () => resolve();
    });
  } catch {
    // Nothing to drop.
  }
}
