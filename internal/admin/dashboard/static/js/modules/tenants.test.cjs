const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function loadTenantsModuleFactory(overrides = {}) {
    const source = fs.readFileSync(path.join(__dirname, 'tenants.js'), 'utf8');
    const context = { console, setTimeout, clearTimeout, ...overrides };
    vm.createContext(context);
    vm.runInContext(source, context);
    return context.dashboardTenantsModule;
}

function createTenantsModule(overrides) {
    const factory = loadTenantsModuleFactory(overrides);
    return factory();
}

test('tenantPayload requires subdomain and name', () => {
    const module = createTenantsModule();
    module.tenantForm = { subdomain: '  acme  ', name: '  Acme Corp  ', plan: ' pro ' };
    assert.equal(JSON.stringify(module.tenantPayload()), JSON.stringify({ subdomain: 'acme', name: 'Acme Corp', plan: 'pro' }));
});

test('tenantPayload rejects empty subdomain or name', () => {
    const module = createTenantsModule();
    module.tenantForm = { subdomain: '', name: 'Acme', plan: '' };
    assert.equal(module.tenantPayload(), null);
    assert.match(module.tenantFormError, /required/);
});

test('fetchTenants loads { tenants: [...] } from /admin/tenants', async () => {
    let requestedURL = null;
    const module = createTenantsModule({
        fetch: async (url) => {
            requestedURL = url;
            return {
                ok: true,
                status: 200,
                json: async () => ({ tenants: [{ id: 't1', subdomain: 'acme', name: 'Acme', status: 'active' }] })
            };
        }
    });
    module.requestOptions = () => ({ headers: {} });
    module.handleFetchResponse = () => true;
    module.isStaleAuthFetchResult = () => false;
    module.renderIconsAfterUpdate = () => {};
    await module.fetchTenants();
    assert.equal(requestedURL, '/admin/tenants');
    assert.equal(module.tenants.length, 1);
    assert.equal(module.tenants[0].subdomain, 'acme');
});

test('fetchTenants sets error on non-ok response', async () => {
    const module = createTenantsModule({
        fetch: async () => ({ ok: false, status: 500 })
    });
    module.requestOptions = () => ({ headers: {} });
    module.handleFetchResponse = () => false;
    module.isStaleAuthFetchResult = () => false;
    await module.fetchTenants();
    assert.match(module.tenantError, /Unable to load tenants/);
});

test('fetchTenants clears stale tenants on non-ok response', async () => {
    const module = createTenantsModule({
        fetch: async () => ({ ok: false, status: 500 })
    });
    module.requestOptions = () => ({ headers: {} });
    module.handleFetchResponse = () => false;
    module.isStaleAuthFetchResult = () => false;
    module.tenants = [{ id: 't1', subdomain: 'acme', name: 'Acme', status: 'active' }];
    await module.fetchTenants();
    assert.equal(module.tenants.length, 0);
    assert.match(module.tenantError, /Unable to load tenants/);
});

async function createSubmitFormModule() {
    const module = createTenantsModule({
        fetch: async () => ({ ok: true, status: 200, json: async () => ({ tenants: [] }) })
    });
    module.requestOptions = () => ({ headers: {} });
    module.handleFetchResponse = () => true;
    module.isStaleAuthFetchResult = () => false;
    module.fetchTenants = async () => {};
    return module;
}

test('submitTenantForm shows "Tenant saved." after editing an existing tenant', async () => {
    const module = await createSubmitFormModule();
    module.tenantEditing = true;
    module.tenantEditingID = 't1';
    module.tenantForm = { subdomain: 'acme', name: 'Acme Corp', plan: '' };
    await module.submitTenantForm();
    assert.equal(module.tenantNotice, 'Tenant saved.');
    assert.equal(module.tenantEditing, false);
    assert.equal(module.tenantEditingID, '');
});

test('submitTenantForm shows "Tenant created." after creating a new tenant', async () => {
    const module = await createSubmitFormModule();
    module.tenantEditing = false;
    module.tenantForm = { subdomain: 'acme', name: 'Acme Corp', plan: '' };
    await module.submitTenantForm();
    assert.equal(module.tenantNotice, 'Tenant created.');
});

test('isPlatformAdmin defaults to true when flag unset', () => {
    const module = createTenantsModule();
    assert.equal(module.isPlatformAdmin, true);
});

test('isPlatformAdmin respects window flag', () => {
    const module = createTenantsModule({
        SMARTROUTER_IS_PLATFORM_ADMIN: false
    });
    assert.equal(module.isPlatformAdmin, false);
});
