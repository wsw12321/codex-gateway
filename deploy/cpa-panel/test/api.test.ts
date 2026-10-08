import {describe, expect, test} from 'bun:test';
import {authorizationURL, callbackFields, importFields} from '../src/api';

describe('Anthropic account credential boundary', () => {
  test('accepts only the exact HTTPS authorization host', () => {
    expect(authorizationURL('https://claude.ai/oauth/authorize?state=test', 'anthropic')).toBe('https://claude.ai/oauth/authorize?state=test');
    for (const url of ['http://claude.ai/oauth', 'https://claude.ai.attacker.test/oauth', 'https://user:secret@claude.ai/oauth', 'https://claude.ai:8443/oauth', 'https://accounts.google.com/oauth']) {
      expect(() => authorizationURL(url, 'anthropic')).toThrow();
    }
  });
  test('extracts only required tokens from Claude Code credentials', () => {
    expect(importFields(JSON.stringify({claudeAiOauth: {refreshToken: 'synthetic-refresh', accessToken: 'synthetic-access', subscriptionType: 'forged', organizationUuid: 'forged', scopes: ['ignored']}, arbitrary: 'ignored'}), 'anthropic')).toEqual({refresh_token: 'synthetic-refresh', access_token: 'synthetic-access'});
    expect(importFields(JSON.stringify({refresh_token: 'synthetic-refresh', access_token: 'synthetic-access', id_token: 'ignored', account_uuid: 'ignored'}), 'anthropic')).toEqual({refresh_token: 'synthetic-refresh', access_token: 'synthetic-access'});
    for (const value of [[], null, {claudeAiOauth: []}, {claudeAiOauth: null}, {claudeAiOauth: {}}]) expect(() => importFields(JSON.stringify(value), 'anthropic')).toThrow();
  });
  test('accepts an access token alone only for Anthropic and validates token bounds and types', () => {
    expect(importFields(JSON.stringify({access_token: 'synthetic-access', id_token: 'ignored'}), 'anthropic')).toEqual({access_token: 'synthetic-access'});
    expect(importFields(JSON.stringify({claudeAiOauth: {accessToken: 'synthetic-access'}}), 'anthropic')).toEqual({access_token: 'synthetic-access'});
    expect(importFields(JSON.stringify({refresh_token: 'synthetic-refresh'}), 'anthropic')).toEqual({refresh_token: 'synthetic-refresh'});
    for (const provider of ['codex', 'antigravity'] as const) expect(() => importFields(JSON.stringify({access_token: 'synthetic-access'}), provider)).toThrow('refresh_token');
    for (const tokens of [{}, {access_token: ' ', refresh_token: ''}, {access_token: 123}, {refresh_token: null, access_token: 'valid'}, {access_token: 'a'.repeat(32769)}, {access_token: '字'.repeat(10923)}]) expect(() => importFields(JSON.stringify(tokens), 'anthropic')).toThrow();
    expect(importFields(JSON.stringify({access_token: 'a'.repeat(32768)}), 'anthropic').access_token.length).toBe(32768);
  });
  test('local callbacks reject duplicate and ambiguous authorization parameters', () => {
    expect(callbackFields('http://localhost:54545/callback?code=synthetic-code&state=synthetic-state')).toEqual({code: 'synthetic-code', state: 'synthetic-state'});
    for (const url of ['https://claude.ai/callback?code=a&state=b', 'http://localhost/callback?code=a&code=b&state=c', 'http://localhost/callback?code=a&state=b&state=c', 'http://localhost/callback?code=a&state=b&error=']) expect(() => callbackFields(url)).toThrow();
  });
});
