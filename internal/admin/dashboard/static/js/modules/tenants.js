(function(global) {
    function dashboardTenantsModule() {
        return {
            isPlatformAdmin: typeof global.SMARTROUTER_IS_PLATFORM_ADMIN !== 'undefined'
                ? !!global.SMARTROUTER_IS_PLATFORM_ADMIN
                : true,
            tenants: [],
            tenantsLoading: false,
            tenantError: '',
            tenantNotice: '',
            tenantFormOpen: false,
            tenantFormSubmitting: false,
            tenantFormError: '',
            tenantEditing: false,
            tenantEditingID: '',
            tenantDisablingID: '',
            tenantFetchPromise: null,
            tenantForm: {
                subdomain: '',
                name: '',
                plan: ''
            },

            defaultTenantForm() {
                return { subdomain: '', name: '', plan: '' };
            },

            tenantPayload() {
                const subdomain = String(this.tenantForm.subdomain || '').trim().toLowerCase();
                const name = String(this.tenantForm.name || '').trim();
                if (!subdomain || !name) {
                    this.tenantFormError = 'Subdomain and name are required.';
                    return null;
                }
                const payload = { subdomain, name };
                const plan = String(this.tenantForm.plan || '').trim();
                if (plan) payload.plan = plan;
                return payload;
            },

            async fetchTenantsPage() {
                if (this.tenantFetchPromise) {
                    return this.tenantFetchPromise;
                }
                this.tenantFetchPromise = this.fetchTenants().finally(() => {
                    this.tenantFetchPromise = null;
                });
                return this.tenantFetchPromise;
            },

            async fetchTenants() {
                this.tenantsLoading = true;
                this.tenantError = '';
                try {
                    const request = this.requestOptions();
                    const res = await fetch('/admin/tenants', request);
                    const handled = this.handleFetchResponse(res, 'tenants', request);
                    if (this.isStaleAuthFetchResult(handled)) {
                        return;
                    }
                    if (!handled) {
                        this.tenants = [];
                        this.tenantError = 'Unable to load tenants.';
                        return;
                    }
                    const payload = await res.json();
                    this.tenants = payload && Array.isArray(payload.tenants) ? payload.tenants : [];
                    if (typeof this.renderIconsAfterUpdate === 'function') {
                        this.renderIconsAfterUpdate();
                    }
                } catch (e) {
                    console.error('Failed to fetch tenants:', e);
                    this.tenants = [];
                    this.tenantError = 'Unable to load tenants.';
                } finally {
                    this.tenantsLoading = false;
                }
            },

            openTenantForm(item) {
                this.tenantEditing = !!item;
                this.tenantEditingID = item ? item.id : '';
                this.tenantFormError = '';
                this.tenantError = '';
                this.tenantNotice = '';
                this.tenantForm = item
                    ? { subdomain: item.subdomain, name: item.name, plan: item.plan || '' }
                    : this.defaultTenantForm();
                this.tenantFormOpen = true;
                if (typeof this.renderIconsAfterUpdate === 'function') {
                    this.renderIconsAfterUpdate();
                }
                if (typeof this.$nextTick === 'function') {
                    this.$nextTick(() => {
                        const refs = this.$refs || {};
                        const input = refs.tenantSubdomainInput;
                        if (input && typeof input.focus === 'function') {
                            input.focus({ preventScroll: true });
                        }
                    });
                }
            },

            closeTenantForm() {
                this.tenantFormOpen = false;
                this.tenantFormSubmitting = false;
                this.tenantFormError = '';
                this.tenantEditing = false;
                this.tenantEditingID = '';
                this.tenantForm = this.defaultTenantForm();
            },

            async submitTenantForm() {
                if (this.tenantFormSubmitting) {
                    return;
                }
                const payload = this.tenantPayload();
                if (!payload) {
                    return;
                }
                this.tenantFormSubmitting = true;
                this.tenantFormError = '';
                this.tenantError = '';
                this.tenantNotice = '';
                try {
                    // Create: POST /admin/tenants; Update: PATCH /admin/tenants/:id
                    const request = this.requestOptions({
                        method: this.tenantEditing ? 'PATCH' : 'POST',
                        body: JSON.stringify(payload)
                    });
                    const res = await fetch(this.tenantEditing
                        ? '/admin/tenants/' + encodeURIComponent(this.tenantEditingID)
                        : '/admin/tenants', request);
                    const handled = this.handleFetchResponse(res, 'tenant', request);
                    if (this.isStaleAuthFetchResult(handled)) {
                        return;
                    }
                    if (!handled) {
                        this.tenantFormError = await this.tenantResponseMessage(res, 'Unable to save tenant.');
                        return;
                    }
                    const editing = this.tenantEditing;
                    this.closeTenantForm();
                    await this.fetchTenants();
                    this.tenantNotice = editing ? 'Tenant saved.' : 'Tenant created.';
                } catch (e) {
                    console.error('Failed to save tenant:', e);
                    this.tenantFormError = 'Unable to save tenant.';
                } finally {
                    this.tenantFormSubmitting = false;
                }
            },

            async disableTenant(item) {
                if (!item || this.tenantDisablingID) {
                    return;
                }
                if (global.confirm && !global.confirm('Disable tenant "' + item.name + '"? This soft-deletes the tenant and blocks its API keys.')) {
                    return;
                }
                this.tenantDisablingID = item.id;
                this.tenantError = '';
                this.tenantNotice = '';
                try {
                    const request = this.requestOptions({ method: 'DELETE' });
                    const res = await fetch('/admin/tenants/' + encodeURIComponent(item.id), request);
                    const handled = this.handleFetchResponse(res, 'tenant disable', request);
                    if (this.isStaleAuthFetchResult(handled)) {
                        return;
                    }
                    if (!handled) {
                        this.tenantError = await this.tenantResponseMessage(res, 'Unable to disable tenant.');
                        return;
                    }
                    await this.fetchTenants();
                    this.tenantNotice = 'Tenant disabled.';
                } catch (e) {
                    console.error('Failed to disable tenant:', e);
                    this.tenantError = 'Unable to disable tenant.';
                } finally {
                    this.tenantDisablingID = '';
                }
            },

            async tenantResponseMessage(res, fallback) {
                try {
                    const payload = await res.json();
                    if (payload && payload.error && payload.error.message) {
                        return payload.error.message;
                    }
                } catch (_) {
                    // Ignore invalid responses; use the fallback message.
                }
                return fallback;
            }
        };
    }

    global.dashboardTenantsModule = dashboardTenantsModule;
})(typeof window !== 'undefined' ? window : globalThis);
