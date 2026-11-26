// Passkeys (WebAuthn) in the browser. The server sends the options as JSON with binary
// fields in base64url; navigator.credentials needs ArrayBuffers, and the answer goes
// back as JSON in the same encoding (the shape the Go library parses). Moorage asks for no
// WebAuthn extensions, so it sends no extension results either: outputs some password
// managers add unasked would only fail the server's strict check.

/** Whether this browser can use passkeys at all (WebAuthn needs a secure context). */
export function passkeysSupported(): boolean {
  return typeof window !== 'undefined' && window.isSecureContext && typeof window.PublicKeyCredential === 'function';
}

function fromB64url(s: string): ArrayBuffer {
  const b64 = s.replace(/-/g, '+').replace(/_/g, '/') + '==='.slice((s.length + 3) % 4);
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out.buffer;
}

function toB64url(buf: ArrayBuffer | null | undefined): string {
  if (!buf) return '';
  const bytes = new Uint8Array(buf);
  let bin = '';
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

type Descriptor = { id: string; type: PublicKeyCredentialType; transports?: AuthenticatorTransport[] };

interface CreationJSON {
  publicKey: Omit<PublicKeyCredentialCreationOptions, 'challenge' | 'user' | 'excludeCredentials'> & {
    challenge: string;
    user: { id: string; name: string; displayName: string };
    excludeCredentials?: Descriptor[];
  };
}

interface RequestJSON {
  publicKey: Omit<PublicKeyCredentialRequestOptions, 'challenge' | 'allowCredentials'> & {
    challenge: string;
    allowCredentials?: Descriptor[];
  };
  mediation?: CredentialMediationRequirement;
}

const descriptors = (list?: Descriptor[]): PublicKeyCredentialDescriptor[] | undefined =>
  list?.map((d) => ({ ...d, id: fromB64url(d.id) }));

/** Plain words for the errors navigator.credentials throws. */
function passkeyError(e: unknown): Error {
  const name = (e as DOMException)?.name ?? '';
  if (name === 'NotAllowedError' || name === 'AbortError') return new Error('The passkey prompt was cancelled or timed out.');
  if (name === 'InvalidStateError') return new Error('This passkey is already registered for your account.');
  if (name === 'SecurityError') return new Error('Passkeys work only at the public address of Moorage. Open Moorage there and try again.');
  return new Error((e as Error)?.message || 'The passkey prompt failed.');
}

/** Runs navigator.credentials.create and returns the answer for the server. */
export async function createPasskey(options: unknown): Promise<unknown> {
  const o = options as CreationJSON;
  const publicKey: PublicKeyCredentialCreationOptions = {
    ...o.publicKey,
    challenge: fromB64url(o.publicKey.challenge),
    user: { ...o.publicKey.user, id: fromB64url(o.publicKey.user.id) },
    excludeCredentials: descriptors(o.publicKey.excludeCredentials),
  };
  let cred: PublicKeyCredential | null;
  try {
    cred = (await navigator.credentials.create({ publicKey })) as PublicKeyCredential | null;
  } catch (e) {
    throw passkeyError(e);
  }
  if (!cred) throw new Error('No passkey was created.');
  const r = cred.response as AuthenticatorAttestationResponse;
  return {
    id: cred.id,
    rawId: toB64url(cred.rawId),
    type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment ?? undefined,
    clientExtensionResults: {},
    response: {
      clientDataJSON: toB64url(r.clientDataJSON),
      attestationObject: toB64url(r.attestationObject),
      transports: typeof r.getTransports === 'function' ? r.getTransports() : undefined,
    },
  };
}

/** Runs navigator.credentials.get and returns the answer for the server. */
export async function getPasskey(options: unknown): Promise<unknown> {
  const o = options as RequestJSON;
  const publicKey: PublicKeyCredentialRequestOptions = {
    ...o.publicKey,
    challenge: fromB64url(o.publicKey.challenge),
    allowCredentials: descriptors(o.publicKey.allowCredentials),
  };
  let cred: PublicKeyCredential | null;
  try {
    cred = (await navigator.credentials.get({ publicKey })) as PublicKeyCredential | null;
  } catch (e) {
    throw passkeyError(e);
  }
  if (!cred) throw new Error('No passkey was chosen.');
  const r = cred.response as AuthenticatorAssertionResponse;
  return {
    id: cred.id,
    rawId: toB64url(cred.rawId),
    type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment ?? undefined,
    clientExtensionResults: {},
    response: {
      clientDataJSON: toB64url(r.clientDataJSON),
      authenticatorData: toB64url(r.authenticatorData),
      signature: toB64url(r.signature),
      userHandle: r.userHandle ? toB64url(r.userHandle) : undefined,
    },
  };
}
