/*
 * Copyright (c) 2014, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package ca.psiphon.psibot;

import android.content.Context;
import android.content.SharedPreferences;
import android.net.VpnService;
import android.preference.PreferenceManager;

import org.json.JSONException;
import org.json.JSONObject;

import java.io.IOException;
import java.util.HashSet;
import java.util.Set;
import java.util.concurrent.CountDownLatch;

import go.psi.Psi;

public class Psiphon extends Psi.PsiphonProvider.Stub {

    private final VpnService mVpnService;
    private final CountDownLatch mTunnelStartedSignal;
    private int mLocalSocksProxyPort;
    private int mLocalHttpProxyPort;
    private Set<String> mHomePages;

    public Psiphon(VpnService vpnService, CountDownLatch tunnelStartedSignal) {
        mVpnService = vpnService;
        mTunnelStartedSignal = tunnelStartedSignal;
    }

    // PsiphonProvider.Notice
    @Override
    public void Notice(String noticeJSON) {
        handleNotice(noticeJSON);
    }

    // PsiphonProvider.BindToDevice
    @Override
    public void BindToDevice(long fileDescriptor) throws Exception {
        if (!mVpnService.protect((int)fileDescriptor)) {
            throw new Exception("protect socket failed");
        }
    }

    // PsiphonProvider.HasNetworkConnectivity
    @Override
    public long HasNetworkConnectivity() {
        // TODO: change to bool return value once gobind supports that type
        return Utils.hasNetworkConnectivity(mVpnService) ? 1 : 0;
    }

    public void start() throws Utils.PsibotError {
        Psi.Stop();

        mLocalSocksProxyPort = 0;
        mLocalHttpProxyPort = 0;
        mHomePages = new HashSet<String>();

        // TODO: supply embedded server list
        String embeddedServerEntryList = "";

        try {
            Psi.Start(loadConfig(mVpnService), embeddedServerEntryList, this);
        } catch (Exception e) {
            throw new Utils.PsibotError("failed to start Psiphon", e);
        }

        Log.addEntry("Psiphon started");
    }

    public void stop() {
        Psi.Stop();
        Log.addEntry("Psiphon stopped");
    }

    public synchronized int getLocalSocksProxyPort() {
        return mLocalSocksProxyPort;
    }

    public synchronized int getLocalHttpProxyPort() {
        return mLocalHttpProxyPort;
    }

    public synchronized Set<String> getHomePages() {
        return mHomePages != null ? new HashSet<String>(mHomePages) : new HashSet<String>();
    }

    private String loadConfig(Context context)
            throws IOException, JSONException, Utils.PsibotError {

        // If we can obtain a DNS resolver for the active network,
        // prefer that for DNS resolution in BindToDevice mode.
        String dnsResolver = null;
        try {
            dnsResolver = Utils.getFirstActiveNetworkDnsResolver(context);
        } catch (Utils.PsibotError e) {
            Log.addEntry("failed to get active network DNS resolver: " + e.getMessage());
            // Proceed with default value in config file
        }

        // Load settings from the raw resource JSON config file and
        // update as necessary. Then write JSON to disk for the Go client.
        String configFileContents = Utils.readInputStreamToString(
                context.getResources().openRawResource(R.raw.psiphon_config));
        JSONObject json = new JSONObject(configFileContents);

        if (dnsResolver != null) {
            json.put("BindToDeviceDnsServer", dnsResolver);
        }

        // On Android, these directories must be set to the app private storage area.
        // The Psiphon library won't be able to use its current working directory
        // and the standard temporary directories do not exist.
        json.put("DataStoreDirectory", mVpnService.getFilesDir());
        json.put("DataStoreTempDirectory", mVpnService.getCacheDir());

        // User-specified settings.
        // Note: currently, validation is not comprehensive, and related errors are
        // not directly parsed.
        SharedPreferences preferences = PreferenceManager.getDefaultSharedPreferences(context);
        json.put("EgressRegion",
                preferences.getString(
                        context.getString(R.string.preferenceEgressRegion),
                        context.getString(R.string.preferenceEgressRegionDefaultValue)));
        json.put("TunnelProtocol",
                preferences.getString(
                        context.getString(R.string.preferenceTunnelProtocol),
                        context.getString(R.string.preferenceTunnelProtocolDefaultValue)));
        json.put("UpstreamHttpProxyAddress",
                preferences.getString(
                        context.getString(R.string.preferenceUpstreamHttpProxyAddress),
                        context.getString(R.string.preferenceUpstreamHttpProxyAddressDefaultValue)));
        json.put("LocalHttpProxyPort",
                Integer.parseInt(
                        preferences.getString(
                                context.getString(R.string.preferenceLocalHttpProxyPort),
                                context.getString(R.string.preferenceLocalHttpProxyPortDefaultValue))));
        json.put("LocalSocksProxyPort",
                Integer.parseInt(
                        preferences.getString(
                                context.getString(R.string.preferenceLocalSocksProxyPort),
                                context.getString(R.string.preferenceLocalSocksProxyPortDefaultValue))));
        json.put("ConnectionWorkerPoolSize",
                Integer.parseInt(
                        preferences.getString(
                                context.getString(R.string.preferenceConnectionWorkerPoolSize),
                                context.getString(R.string.preferenceConnectionWorkerPoolSizeDefaultValue))));
        json.put("TunnelPoolSize",
                Integer.parseInt(
                        preferences.getString(
                                context.getString(R.string.preferenceTunnelPoolSize),
                                context.getString(R.string.preferenceTunnelPoolSizeDefaultValue))));
        json.put("PortForwardFailureThreshold",
                Integer.parseInt(
                        preferences.getString(
                                context.getString(R.string.preferencePortForwardFailureThreshold),
                                context.getString(R.string.preferencePortForwardFailureThresholdDefaultValue))));

        return json.toString();
    }

    private synchronized void handleNotice(String noticeJSON) {
        try {
            JSONObject notice = new JSONObject(noticeJSON);
            String noticeType = notice.getString("noticeType");
            if (noticeType.equals("Tunnels")) {
                int count = notice.getJSONObject("data").getInt("count");
                if (count == 1) {
                    mTunnelStartedSignal.countDown();
                }
            } else if (noticeType.equals("ListeningSocksProxyPort")) {
                mLocalSocksProxyPort = notice.getJSONObject("data").getInt("port");
            } else if (noticeType.equals("ListeningHttpProxyPort")) {
                mLocalHttpProxyPort = notice.getJSONObject("data").getInt("port");
            } else if (noticeType.equals("Homepage")) {
                mHomePages.add(notice.getJSONObject("data").getString("url"));
            }
            String displayNotice = noticeType + " " + notice.getJSONObject("data").toString();
            android.util.Log.d("PSIPHON", displayNotice);
            Log.addEntry(displayNotice);
        } catch (JSONException e) {
            // Ignore notice
        }
    }
}
