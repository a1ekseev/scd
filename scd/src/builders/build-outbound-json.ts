import type { JsonOutbound, NormalizedOutbound } from '../types.ts';

export function buildOutboundJson(entry: NormalizedOutbound): JsonOutbound {
  const user: { id: string; encryption: 'none'; flow?: string } = {
    id: entry.uuid,
    encryption: 'none',
  };
  if (entry.flow) {
    user.flow = entry.flow;
  }

  const streamSettings: Record<string, unknown> = {
    network: entry.network,
    security: entry.security,
  };

  if (entry.security === 'tls') {
    const tlsSettings: Record<string, unknown> = {};
    if (entry.sni) {
      tlsSettings.serverName = entry.sni;
    }
    if (entry.alpn.length > 0) {
      tlsSettings.alpn = entry.alpn;
    }
    if (entry.fingerprint) {
      tlsSettings.fingerprint = entry.fingerprint;
    }
    if (entry.allowInsecure !== undefined) {
      tlsSettings.allowInsecure = entry.allowInsecure;
    }
    streamSettings.tlsSettings = tlsSettings;
  }

  if (entry.security === 'reality') {
    streamSettings.realitySettings = {
      serverName: entry.sni,
      fingerprint: entry.fingerprint,
      publicKey: entry.publicKey,
      shortId: entry.shortId,
    };
  }

  if (entry.network === 'ws') {
    const headers: Record<string, string> = {};
    if (entry.host) {
      headers.Host = entry.host;
    }
    streamSettings.wsSettings = {
      path: entry.path ?? '/',
      ...(Object.keys(headers).length > 0 ? { headers } : {}),
    };
  }

  return {
    tag: entry.tag,
    protocol: 'vless',
    settings: {
      vnext: [
        {
          address: entry.address,
          port: entry.port,
          users: [user],
        },
      ],
    },
    streamSettings,
  };
}
