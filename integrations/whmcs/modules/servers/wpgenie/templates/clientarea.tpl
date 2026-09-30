<div class="panel panel-default">
    <div class="panel-heading"><h3 class="panel-title">WordPress hosting</h3></div>
    <table class="table">
        <tr><td>Plan</td><td>{$wpgPlan|escape}</td></tr>
        <tr><td>Status</td><td>{$wpgStatus|escape}</td></tr>
        <tr><td>Sites</td><td>{$wpgSites|escape}</td></tr>
        <tr><td>Disk</td><td>{$wpgDisk|escape}</td></tr>
        <tr><td>Bandwidth this month</td><td>{$wpgBandwidth|escape}</td></tr>
        {if $wpgBurst}<tr><td>Burst minutes</td><td>{$wpgBurst|escape}</td></tr>{/if}
    </table>
    <div class="panel-body">
        <a class="btn btn-primary" href="clientarea.php?action=productdetails&amp;id={$serviceid}&amp;dosinglesignon=1">Log in to WPGenie</a>
    </div>
</div>
